/*
Copyright 2022 The Kubernetes Authors.
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
	NameIndex = "name"
)

func NewK8sClient() (*clientset.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return clientset.NewForConfig(config)
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

// ChannelIsOpen checks if the channel is open.
func ChannelIsOpen(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return false
	default:
	}

	return true
}
