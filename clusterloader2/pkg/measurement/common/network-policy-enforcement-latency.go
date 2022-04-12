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

package common

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"

	// "k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
	"k8s.io/perf-tests/clusterloader2/pkg/execservice"
	"k8s.io/perf-tests/clusterloader2/pkg/measurement"
	"k8s.io/perf-tests/clusterloader2/pkg/util"
)

const (
	networkPolicyEnforcementLatencyName = "NetworkPolicyEnforcementLatency"
	testLabelSelector                   = "test = net-policy-latency"
	svcReachedTimeoutDuration           = 1 * time.Minute
	waitForReadyTimeoutDuration         = 1 * time.Minute
	maxRetries                          = 10
	retryBackOffTime                    = 10 * time.Second
	podPort                             = 80
	testLabelKey                        = "test"
)

func init() {
	if err := measurement.Register(networkPolicyEnforcementLatencyName, createnetPolicyLatencyMeasurement); err != nil {
		klog.Fatalf("Cannot register %s: %v", networkPolicyEnforcementLatencyName, err)
	}
}

func createnetPolicyLatencyMeasurement() measurement.Measurement {
	return &netPolicyLatencyMeasurement{}
}

type netPolicyLatencyMeasurement struct {
	client        clientset.Interface
	labelSelector string
	// Maps service name to the duration it took to reach it
	// after network policy was updated to allow it.
	podReachedMap   map[string]time.Time
	mapLock         sync.Mutex
	podCount        int
	trafficStopChan chan struct{}
}

// Available actions:
// 1. start - Begin sending traffic
// 2. waitForReady - Wait until all of the services are reached, or time limit is exceeded
// 3. gather - Report latency end time (service reached) - start time (network policy updated)
func (s *netPolicyLatencyMeasurement) Execute(config *measurement.Config) ([]measurement.Summary, error) {
	action, err := util.GetString(config.Params, "action")
	if err != nil {
		return nil, err
	}

	s.labelSelector, err = util.GetString(config.Params, "labelSelector")
	if err != nil {
		return nil, err
	}

	switch action {
	case "start":
		s.client = config.ClusterFramework.GetClientSets().GetClient()
		return nil, s.start()
	case "waitForReady":
		return nil, s.waitForReady()
	case "gather":
		return s.gather()
	default:
		return nil, fmt.Errorf("unknown action %v", action)
	}
}

func (s *netPolicyLatencyMeasurement) start() error {
	ctx := context.Background()
	//serviceList, err := s.client.CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: s.labelSelector})
	//if err != nil {
	//	return fmt.Errorf("cannot list services for label selector %q: %v", s.labelSelector, err)
	//}

	podList, err := s.client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: s.labelSelector})
	if err != nil {
		return fmt.Errorf("failed to list pods for label selector %q: %v", s.labelSelector, err)
	}

	s.podCount = len(podList.Items)
	s.trafficStopChan = make(chan struct{})
	s.podReachedMap = make(map[string]time.Time)

	for _, pod := range podList.Items {
		go s.recordFirstTimeForReachingPod(ctx, pod.Status.PodIP, pod.Labels[testLabelKey])
	}

	return nil
}

func (s *netPolicyLatencyMeasurement) recordFirstTimeForReachingPod(ctx context.Context, podIP, label string) {
	// Get pod with retry, and use the same pod until the service is reached.
	pod, err := execServicePod()
	if err != nil {
		klog.Errorf("Cannot get exec service pod for reaching pod %q with label %q: %v", podIP, label, err)
		return
	}

	cmd := fmt.Sprintf("curl %s:%d", podIP, podPort) // --connect-timeout default is 60s
	klog.Infof("Starting to send traffic to %s:%s service", podIP, podPort)

	for {
		select {
		case <-s.trafficStopChan:
			return
		default:
			out, err := execservice.RunCommand(pod, cmd)
			if err == nil {
				reachedTime := time.Now()
				// The name of the network policy that allows ingress
				// for the pods, must match the name of the service.
				//networkPolicy, err := networkPolicy(ctx, s.client, svc.GetName(), svc.GetNamespace())
				//if err != nil {
				//	klog.Errorf("Failed to get network policy %q: %v", svc.GetName(), err)
				//	return
				//}

				s.mapLock.Lock()
				s.podReachedMap[label] = reachedTime //endTime.Sub(startTime) // Need label here to be able to map networkPolicy to a pod.
				size := len(s.podReachedMap)
				s.mapLock.Unlock()

				klog.Infof("Pod %q successfully reached. latencyMap size = %d", podIP, size)
				return
			} else {
				klog.Infof("Command %q unsuccessful message: %s, error: %v", cmd, out, err)
			}

			time.Sleep(pingBackoff)
		}
	}
}

func execServicePod() (*corev1.Pod, error) {
	var err error
	var pod *corev1.Pod

	klog.Info("Getting an exec service pod for sending traffic to service")

	for retries := 0; retries < maxRetries; retries++ {
		pod, err = execservice.GetPod()
		if err == nil {
			return pod, nil
		}

		klog.Warningf("Call to execservice.GetPod() ended with error: %v", err)
		time.Sleep(retryBackOffTime)
	}

	return nil, fmt.Errorf("failed to execservice.GetPod() after %d retries for service: %v", maxRetries, err)
}

func networkPolicyList(client clientset.Interface, labelSelector string) ([]networkingv1.NetworkPolicy, error) {
	var err error
	var networkPolicyList []networkingv1.NetworkPolicy

	ctx := context.Background()
	klog.Infof("Getting network policy list for %q labelSelector", labelSelector)

	for retries := 0; retries < maxRetries; retries++ {
		npList, err := client.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err == nil {
			networkPolicyList = npList.Items
			break
		}
		klog.Errorf("Retrying to get network policy list for %q labelSelector: %v", labelSelector, err)

		time.Sleep(retryBackOffTime)
	}

	return networkPolicyList, err
}

func (s *netPolicyLatencyMeasurement) waitForReady() error {
	startTime := time.Now()
	verifyInterval := 5 * time.Second

	defer close(s.trafficStopChan)

	for time.Now().Sub(startTime) < waitForReadyTimeoutDuration {
		s.mapLock.Lock()
		mapSize := len(s.podReachedMap)
		s.mapLock.Unlock()

		klog.Infof("Reached %d/%d pods", mapSize, s.podCount)
		if mapSize == s.podCount {
			return nil
		}

		time.Sleep(verifyInterval)
	}

	return fmt.Errorf("failed to reach all of the services within %v", waitForReadyTimeoutDuration)
}

func (s *netPolicyLatencyMeasurement) gather() ([]measurement.Summary, error) {
	results, err := s.prepareLatencyResults()
	if err != nil {
		return nil, fmt.Errorf("prepareLatencyResults() failed: %v", err)
	}

	content, err := util.PrettyPrintJSON(results)
	if err != nil {
		return nil, err
	}

	summaries := []measurement.Summary{measurement.CreateSummary(networkPolicyEnforcementLatencyName, "json", content)}
	fmt.Println(summaries) // test - to-remove
	return summaries, nil
}

func (s *netPolicyLatencyMeasurement) prepareLatencyResults() (map[string]string, error) {
	networkPolicies, err := networkPolicyList(s.client, s.labelSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to get network policy list: %v", err)
	}
	klog.Infof("Retrieved %d network policies", len(networkPolicies))

	neverReachedCount := 0
	var latencyList []time.Duration

	for _, policy := range networkPolicies {
		testLabelValue, ok := policy.Labels[testLabelKey]
		if !ok {
			klog.Warningf("Policy %q does not have label for key %q", policy.GetName(), testLabelKey)
			neverReachedCount++
			continue
		}

		s.mapLock.Lock()
		endTime, ok := s.podReachedMap[testLabelValue]
		s.mapLock.Unlock()

		if !ok {
			klog.Warningf("Pod with %s = %s label not found", testLabelKey, testLabelValue)
			neverReachedCount++
			continue
		}

		startTime := policy.GetCreationTimestamp().Time

		latency := endTime.Sub(startTime)
		latencyList = append(latencyList, latency)
	}

	sort.Slice(latencyList, func(i, j int) bool { return latencyList[i] < latencyList[j] })
	latencyResults := make(map[string]string)

	if len(latencyList) > 0 {
		size := len(latencyList)
		klog.Infof("Number of reached pods: %d", size)

		// 50th percentile latency.
		idx := size * 50 / 100
		latencyResults["50th"] = latencyList[idx].String()
		// 90th percentile latency.
		idx = size * 90 / 100
		latencyResults["90th"] = latencyList[idx].String()
		// 99th percentile latency.
		idx = size * 99 / 100
		latencyResults["99th"] = latencyList[idx].String()
	}

	latencyResults["NeverReachedCount"] = strconv.Itoa(neverReachedCount)

	return latencyResults, nil
}

// String returns a string representation of the measurement.
func (s *netPolicyLatencyMeasurement) String() string {
	return networkPolicyEnforcementLatencyName
}

// Dispose cleans up after the measurement.
func (s *netPolicyLatencyMeasurement) Dispose() {
	select {
	case <-s.trafficStopChan:
		break
	default:
		close(s.trafficStopChan)
	}
}
