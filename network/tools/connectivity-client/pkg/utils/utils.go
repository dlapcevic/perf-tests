/*
Copyright 2023 The Kubernetes Authors.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"bytes"
	"fmt"
	"os/exec"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	NameIndex           = "name"
	MaximumRetryBackoff = 10 * time.Minute
)

// NewK8sClient returns a K8s client for the K8s cluster where this application
// runs inside a pod.
func NewK8sClient() (*clientset.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return clientset.NewForConfig(config)
}

// Retry runs a function until it succeeds, with specified number of attempts
// and base for exponential backoff.
func Retry(attempts int, backoff time.Duration, fn func() error) error {
	if err := fn(); err != nil {
		newBackoff := backoff * 2
		if attempts > 1 && newBackoff < MaximumRetryBackoff {
			time.Sleep(backoff)
			return Retry(attempts-1, newBackoff, fn)
		}
		return err
	}

	return nil
}

// InformerSynced verifies that the provided sync function is successful.
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

// RunCommand executes a command based on the provided string slice. The command
// is constructed by taking the first element as the command name, and then all
// the subsequent elements as arguments to that command.
func RunCommand(cmd []string) (string, error) {
	var stdout, stderr bytes.Buffer
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		return stderr.String(), err
	}
	return stdout.String(), nil
}

// ChannelIsOpen checks if the channel is open.
func ChannelIsOpen(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return false
	default:
	}

	return true
}
