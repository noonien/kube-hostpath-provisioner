package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"syscall"
	"time"

	"github.com/urfave/cli"

	"github.com/kubernetes-incubator/external-storage/lib/controller"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
)

const (
	provisionerName               = "hostpath.external-storage.incubator.kubernetes.io"
	provisionerIdentityAnnotation = provisionerName + "/ID"

	resyncPeriod              = 15 * time.Second
	exponentialBackOffOnError = false
	failedRetryThreshold      = 5
	leasePeriod               = controller.DefaultLeaseDuration
	retryPeriod               = controller.DefaultRetryPeriod
	renewDeadline             = controller.DefaultRenewDeadline
	termLimit                 = controller.DefaultTermLimit
)

func main() {
	app := cli.NewApp()
	app.Name = "kube-hostpath-provisioner"
	app.Version = "1.0.0"
	app.Usage = "dynamically provisions hostPath PersistentVolumes"
	app.Action = Run
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "node-name",
			Usage:  "node name",
			EnvVar: "KHP_NODE_NAME",
		},
		cli.StringFlag{
			Name:   "root",
			Value:  "/tmp/hostpath-provisioner",
			Usage:  "hostPath root",
			EnvVar: "KHP_ROOT",
		},
	}

	app.Run(os.Args)
}

func Run(c *cli.Context) error {
	nodeName := c.String("node-name")
	if nodeName == "" {
		return cli.NewExitError("node-name is mandatory", 1)
	}

	root := c.String("root")
	if root == "" {
		return cli.NewExitError("root path is mandatory", 1)
	}

	p := NewHostPathProvisioner(nodeName, root)
	err := Start(p)
	if err != nil {
		return cli.NewExitError(err.Error(), 1)
	}

	return nil
}

func Start(p *hostPathProvisioner) error {
	syscall.Umask(0)

	// Create an InClusterConfig and use it to create a client for the controller
	// to use to communicate with Kubernetes
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to create config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("Failed to create client: %v", err)
	}

	serverVersion, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("Error getting server version: %v", err)
	}

	pc := controller.NewProvisionController(
		clientset,
		resyncPeriod,
		provisionerName,
		p,
		serverVersion.GitVersion,
		exponentialBackOffOnError,
		failedRetryThreshold, leasePeriod, renewDeadline, retryPeriod, termLimit)
	pc.Run(wait.NeverStop)
	return nil
}

type hostPathProvisioner struct {
	// Identity of this hostPathProvisioner, set to node's name. Used to identify
	// "this" provisioner's PVs.
	Identity string

	// The directory to create PV-backing directories in
	Root string
}

func NewHostPathProvisioner(nodeName, root string) *hostPathProvisioner {
	return &hostPathProvisioner{
		Identity: nodeName,
		Root:     root,
	}
}

// Provision creates a storage asset and returns a PV object representing it.
func (p *hostPathProvisioner) Provision(options controller.VolumeOptions) (*v1.PersistentVolume, error) {
	path := path.Join(p.Root, options.PVName)

	if err := os.MkdirAll(path, 0777); err != nil {
		return nil, err
	}

	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: options.PVName,
			Annotations: map[string]string{
				provisionerIdentityAnnotation: p.Identity,
			},
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: options.PersistentVolumeReclaimPolicy,
			AccessModes:                   options.PVC.Spec.AccessModes,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): options.PVC.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)],
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: path,
				},
			},
		},
	}

	return pv, nil
}

// Delete removes the storage asset that was created by Provision represented
// by the given PV.
func (p *hostPathProvisioner) Delete(volume *v1.PersistentVolume) error {
	ann, ok := volume.Annotations[provisionerIdentityAnnotation]
	if !ok {
		return errors.New("identity annotation not found on PV")
	}
	if ann != p.Identity {
		return &controller.IgnoredError{Reason: "identity annotation on PV does not match ours"}
	}

	path := path.Join(p.Root, volume.Name)
	if err := os.RemoveAll(path); err != nil {
		return err
	}

	return nil
}
