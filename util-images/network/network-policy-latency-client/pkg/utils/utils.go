package utils

import (
	"bytes"
	"fmt"
	"os/exec"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	MatchNetPolicyByTypeLabelKey = "type"
	ApiserverPolicyBaseName      = "podcreation-egress-apiserver"
	DefaultMaxRetries            = 5
	DefaultBackoffDuration       = 1 * time.Second
	InformerResyncTime           = 0 * time.Second
	CiliumEndpointSliceGroup     = "cilium.io"
	// CiliumEndpointSliceVersions represents the API version of CiliumEndpointSlice API.
	CiliumEndpointSliceVersions = "v2alpha1"
	// CiliumEndpointSliceResourceName represents the resource name of CiliumEndpointSlice API.
	CiliumEndpointSliceResourceName = "ciliumendpointslices"
	// NameIndex is the lookup name for the index function that indexes by the
	// object's name.
	NameIndex string = "name"
)

var (
	// CiliumEndpointSliceGVR represents the GroupVersionResource for v2alpha1 CiliumEndpointSlice.
	CiliumEndpointSliceGVR = schema.GroupVersionResource{Group: CiliumEndpointSliceGroup, Version: CiliumEndpointSliceVersions, Resource: CiliumEndpointSliceResourceName}
)

func NewK8sClient() (*clientset.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return clientset.NewForConfig(config)
}

func NewDynamicClient() (dynamic.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(config)
}

func Retry(attempts int, sleep time.Duration, fn func() error) error {
	if err := fn(); err != nil {
		if attempts--; attempts > 0 {
			time.Sleep(sleep)
			return Retry(attempts, 2*sleep, fn)
		}
		return err
	}

	return nil
}

func InformerSynced(syncFunc func() bool, informerName string) error {
	yes := syncFunc()
	if yes {
		return nil
	}

	return fmt.Errorf("failed to sync informer %s", informerName)
}

// MetaNameIndexFunc is an index function that indexes based on an object's name.
func MetaNameIndexFunc(obj interface{}) ([]string, error) {
	m, err := meta.Accessor(obj)
	if err != nil {
		return []string{""}, fmt.Errorf("object has no meta: %v", err)
	}
	return []string{m.GetName()}, nil
}

func RunCommand(cmd []string) (string, error) {
	var stdout, stderr bytes.Buffer
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		return stderr.String(), err
	}
	return stdout.String(), nil
}
