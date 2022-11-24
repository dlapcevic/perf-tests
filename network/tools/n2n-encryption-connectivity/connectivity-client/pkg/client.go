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

package client

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/perf-tests/network/tools/connectivity-client/pkg/utils"
)

type TestClient struct {
	MainStopChan chan os.Signal
	Config       *config
	k8sClient    *clientset.Clientset

	informerStopChan chan struct{}
}

type config struct {
	targetNamespace string
	targetPort      int
	qps, timeout    int
}

type targetSpec struct {
	ip, name, namespace string
	startTime           time.Time
}

func CreateConfig() *config {
	config := config{}

	flag.StringVar(&config.targetNamespace, "targetNamespace", "default", "The namespace of the target services")
	flag.IntVar(&config.targetPort, "targetPort", 80, "The port of the target services")
	flag.IntVar(&config.qps, "qps", 1, "Rate of sending requests to services")
	flag.IntVar(&config.timeout, "timeout", 1, "Request timeout")
	flag.Parse()

	return &config
}

func NewTestClient(config *config, mainStopChan chan os.Signal) *TestClient {
	return &TestClient{
		Config:       config,
		MainStopChan: mainStopChan,
	}
}

func (c *TestClient) Run() {
	if err := c.initialize(); err != nil {
		log.Fatalf("Failed to initialize: %v", err)
		return
	}

	qpsSleepDuration := (1 * time.Second) / time.Duration(c.Config.qps)

	for {
		serviceList, err := c.targetServiceList()
		if err != nil {
			log.Fatalf("Failed to get service list: %v", err)
		}

		for _, service := range serviceList {
			target := &targetSpec{
				ip:        service.Spec.ClusterIP,
				name:      service.GetName(),
				namespace: service.GetNamespace(),
				startTime: time.Now(),
			}

			if target.name == "kubernetes" {
				continue
			}

			go c.sendRequest(target)

			time.Sleep(qpsSleepDuration)
		}
	}
}

func (c *TestClient) initialize() error {
	log.Printf("Starting connectivity test client! Time: %v", time.Now())
	if err := c.verifyConfig(); err != nil {
		return fmt.Errorf("failed to verify configuration, error: %v", err)
	}

	k8sClient, err := utils.NewK8sClient()
	if err != nil {
		return fmt.Errorf("failed to create k8s client, error: %v", err)
	}

	c.k8sClient = k8sClient

	return nil
}

func (c *TestClient) sendRequest(target *targetSpec) {
	request := fmt.Sprintf("curl --connect-timeout %d %s:%d", c.Config.timeout, target.ip, c.Config.targetPort)
	command := []string{"sh", "-c", "-x", request}
	log.Printf("Sending request %q", request)

	_, err := utils.RunCommand(command)
	latency := time.Now().Sub(target.startTime)

	if err == nil {
		log.Printf("Successfully reached endpoint %s for service %s/%s after %v", target.ip, target.name, target.namespace, latency)
	} else {
		log.Printf("Failed to reached endpoint %s for service %s/%s after %v, error: %v", target.ip, target.name, target.namespace, latency, err)
	}
}

func (c *TestClient) verifyConfig() error {
	if c.Config.qps < 1 {
		return fmt.Errorf("invalid value for qps: %d", c.Config.qps)
	}

	// Log all test client config flags.
	fmt.Printf("--qps=%d\n", c.Config.qps)

	return nil
}

func (c *TestClient) targetServiceList() ([]corev1.Service, error) {
	serviceList, err := c.k8sClient.CoreV1().Services(c.Config.targetNamespace).List(context.Background(), metav1.ListOptions{Limit: 100})
	if err != nil {
		return nil, fmt.Errorf("failed to list services in namespace %q: %v", c.Config.targetNamespace, err)
	}

	if len(serviceList.Items) == 0 {
		return nil, fmt.Errorf("no services listed in namespace %q: %v", c.Config.targetNamespace, err)
	}

	return serviceList.Items, nil
}

// enterIdleState is used for preventing the pod from completing and then
// getting restarted. The test client enters the idle state as soon as it has
// finished the setup and has run the required processes (watchers and
// goroutines).
func (c *TestClient) enterIdleState() {
	log.Printf("Going into idle state")
	select {
	case <-c.MainStopChan:
		c.stopInformerChan()
		return
	}
}

func (c *TestClient) stopInformerChan() {
	//if c.Config.testPodCreation {
	//	close(c.informerStopChan)
	//}

	_, ok := <-c.informerStopChan
	if ok {
		close(c.informerStopChan)
	}
}
