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

package client

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/perf-tests/network/tools/connectivity-client/pkg/metrics"
	"k8s.io/perf-tests/network/tools/connectivity-client/pkg/utils"
)

type TestClient struct {
	MainStopChan  chan os.Signal
	Config        *config
	metricsServer *http.Server
	k8sClient     *clientset.Clientset

	// Fields used by policy creation test.
	policyCreatedTime *timeWithLock

	// Fields used by pod creation test.
	informerStopChan     chan struct{}
	podInformer          cache.SharedIndexInformer
	podCreationWorkQueue *workqueue.Type
	seenPods             *timeMapWithLock
}

type config struct {
	// The namespace of test client pods.
	testClientNamespace string
	// The label selector of target pods to send requests to.
	targetLabelSelector string
	// The namespace of target pods to send requests to.
	targetNamespace string
	// The port number of target pods to send requests to.
	targetPort int
	// The expected number of target pods to send requests to.
	expectedTargets int
	// The name of the egress policy that allows traffic from test client pods to
	// target pods.
	allowPolicyName string
	// The port number where Prometheus metrics are exposed.
	metricsPort int
	// Specifies whether to test latency for pod creation
	testPodCreation bool
	// The number of workers to process the pod watch events in the workerQueue.
	workerCount  int
	testTypeName string
}

type timeMapWithLock struct {
	mp   map[string]time.Time
	lock sync.RWMutex
}

type timeWithLock struct {
	time *time.Time
	lock sync.RWMutex
}

type targetSpec struct {
	ip, name, namespace string
	startTime           time.Time
}

// CreateConfig creates a configuration object for test client based on the
// command flags.
func CreateConfig() *config {
	config := config{}

	flag.StringVar(&config.testClientNamespace, "testClientNamespace", "", "The namespace of test client pods")
	flag.StringVar(&config.targetLabelSelector, "targetLabelSelector", "", "The label selector of target pods to send requests to")
	flag.StringVar(&config.targetNamespace, "targetNamespace", "", "The namespace of target pods to send requests to")
	flag.IntVar(&config.targetPort, "targetPort", 0, "The port number of target pods to send requests to")
	flag.IntVar(&config.expectedTargets, "expectedTargets", 100, "The expected number of target pods to send requests to")
	flag.StringVar(&config.allowPolicyName, "allowPolicyName", "", "The name of the egress policy that allows traffic from test client pods to target pods")
	flag.IntVar(&config.metricsPort, "metricsPort", 9154, "The port number where Prometheus metrics are exposed")
	flag.BoolVar(&config.testPodCreation, "testPodCreation", false, "Specifies whether to test latency for pod creation")
	flag.IntVar(&config.workerCount, "workerCount", 10, "The number of workers to process the pod watch events in the workerQueue")
	flag.Parse()

	return &config
}

// NewTestClient creates a new test client for the provided config.
func NewTestClient(config *config, mainStopChan chan os.Signal) *TestClient {
	return &TestClient{
		Config:       config,
		MainStopChan: mainStopChan,
	}
}

// Run verifies the test client configuration, starts the metrics server, and
// runs the network policy enforcement latency test based on the configuration.
func (c *TestClient) Run() {
	if err := c.initialize(); err != nil {
		log.Printf("Failed to initialize: %v", err)
		c.enterIdleState()
		return
	}

	c.metricsServer = metrics.StartMetricsServer(fmt.Sprintf(":%d", c.Config.metricsPort))
	metrics.RegisterMetrics(c.Config.testPodCreation)
	defer func(metricsServer *http.Server, ctx context.Context) {
		if err := metricsServer.Shutdown(ctx); err != nil {
			log.Printf("Metrics server shutdown returned error - %v", err)
		}
	}(c.metricsServer, context.TODO())

	if c.Config.testPodCreation {
		if err := c.startMeasurePodCreation(); err != nil {
			log.Printf("Pod creation test failed, error: %v\n", err)
		}
	} else {
		if err := c.startMeasureNetPolicyCreation(); err != nil {
			log.Printf("Pod creation test failed, error: %v\n", err)
		}
	}
	// Prevents from pod restarting, after everything is finished.
	// The test will run only once, when the application is deployed.
	// Restart the application to rerun the test.
	c.enterIdleState()
}

// initialize verifies the config and instantiates the objects required for the
// test to run.
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
	c.podCreationWorkQueue = workqueue.New()

	return nil
}

// verifyConfig checks that all the mandatory test client config fields are
// populated accordingly.
func (c *TestClient) verifyConfig() error {
	if len(c.Config.testClientNamespace) == 0 {
		return fmt.Errorf("testClientNamespace parameter is not specified")
	}

	if len(c.Config.targetLabelSelector) == 0 {
		return fmt.Errorf("targetLabelSelector parameter is not specified")
	}

	if len(c.Config.targetNamespace) == 0 {
		return fmt.Errorf("targetNamespace parameter is not specified")
	}

	if c.Config.targetPort == 0 {
		return fmt.Errorf("targetPort parameter is not specified")
	}

	if c.Config.workerCount < 1 || c.Config.workerCount > 100 {
		return fmt.Errorf("workerCount value must be between 1 and 100, but it is %v", c.Config.workerCount)
	}

	if c.Config.testPodCreation {
		c.Config.testTypeName = "pod-creation-enforcement-latency"
	} else {
		if len(c.Config.allowPolicyName) == 0 {
			return fmt.Errorf("allowPolicyName is not specified for policy creation test")
		}
		c.Config.testTypeName = "policy-creation-enforcement-latency"
	}

	fmt.Printf("Connectivity test client mode: %q\n", c.Config.testTypeName)

	// Log all test client config flags.
	fmt.Println("Connectivity test client parameters:")
	fmt.Printf("--testClientNamespace=%s\n", c.Config.testClientNamespace)
	fmt.Printf("--targetLabelSelector=%s\n", c.Config.targetLabelSelector)
	fmt.Printf("--targetNamespace=%s\n", c.Config.targetNamespace)
	fmt.Printf("--targetPort=%d\n", c.Config.targetPort)
	fmt.Printf("--expectedTargets=%d\n", c.Config.expectedTargets)
	fmt.Printf("--allowPolicyName=%s\n", c.Config.allowPolicyName)
	fmt.Printf("--metricsPort=%d\n", c.Config.metricsPort)
	fmt.Printf("--testPodCreation=%t\n", c.Config.testPodCreation)
	fmt.Printf("--workerCount=%d\n", c.Config.workerCount)

	return nil
}

// startMeasureNetPolicyCreation runs the network policy enforcement latency
// test for network policy creation.
func (c *TestClient) startMeasureNetPolicyCreation() error {
	log.Println("Starting to measure network policy enforcement latency after network policy creation")
	podList, err := c.targetPodList()
	if err != nil {
		return fmt.Errorf("failed to get the pod list, error: %v\n", err)
	}
	log.Printf("%d pods listed\n", len(podList))

	wg := sync.WaitGroup{}
	c.policyCreatedTime = &timeWithLock{lock: sync.RWMutex{}}

	// Keep sending requests to all pods until all of them are reached.
	for _, pod := range podList {
		target := &targetSpec{
			ip:        pod.Status.PodIP,
			name:      pod.GetName(),
			namespace: pod.GetNamespace(),
		}

		wg.Add(1)
		go func() {
			c.recordFirstSuccessfulRequest(target, false)
			wg.Done()
		}()
	}

	wg.Wait()
	return nil
}

// targetPodList returns a list of pods based on the test client config, for
// target namespace and target label selector, with a limit to the listed pods.
func (c *TestClient) targetPodList() ([]corev1.Pod, error) {
	podList, err := c.k8sClient.CoreV1().Pods(c.Config.targetNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: c.Config.targetLabelSelector, Limit: int64(c.Config.expectedTargets)})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods for selector %q in namespace %q: %v", c.Config.targetLabelSelector, c.Config.targetNamespace, err)
	}

	if len(podList.Items) == 0 {
		return nil, fmt.Errorf("no pods listed for selector %q in namespace %q: %v", c.Config.targetLabelSelector, c.Config.targetNamespace, err)
	}

	return podList.Items, nil
}

// startMeasurePodCreation runs the network policy enforcement latency test for
// pod creation.
func (c *TestClient) startMeasurePodCreation() error {
	log.Println("Starting to measure pod creation reachability latency")
	c.informerStopChan = make(chan struct{})
	c.seenPods = &timeMapWithLock{mp: make(map[string]time.Time), lock: sync.RWMutex{}}

	// Run workers.
	log.Printf("Starting %d workers for parallel processing of pod add and update events\n", c.Config.workerCount)
	for i := 0; i < c.Config.workerCount; i++ {
		go c.processPodCreationEvents()
	}

	return c.createPodInformer()
}

// createPodInformer creates a pod informer based on the test client config, for
// target namespace and target label selector, and enqueues the events to the
// pod creation work queue.
func (c *TestClient) createPodInformer() error {
	log.Printf("Creating PodWatcher for namespace %q, labelSelector %q\n", c.Config.targetNamespace, c.Config.targetLabelSelector)

	listWatch := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			options.LabelSelector = c.Config.targetLabelSelector
			return c.k8sClient.CoreV1().Pods(c.Config.targetNamespace).List(context.TODO(), options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			options.LabelSelector = c.Config.targetLabelSelector
			return c.k8sClient.CoreV1().Pods(c.Config.targetNamespace).Watch(context.TODO(), options)
		},
	}

	handleEvent := func(obj interface{}) {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			log.Printf("handleEvent() failed to convert newObj (%T) to *corev1.Pod\n", obj)
			return
		}

		c.podCreationWorkQueue.Add(pod.GetName())
	}

	informer := cache.NewSharedIndexInformer(listWatch, nil, 0, cache.Indexers{utils.NameIndex: utils.MetaNameIndexFunc})
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			handleEvent(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			handleEvent(newObj)
		},
	})

	c.podInformer = informer
	go informer.Run(c.informerStopChan)

	err := utils.Retry(10, 500*time.Millisecond, func() error {
		return utils.InformerSynced(informer.HasSynced, "pod informer")
	})

	return err
}

// processPodCreationEvents is run by each worker to process the incoming pod
// watch events, by sending traffic to the pods that have IP address assigned,
// until a first successful response.
func (c *TestClient) processPodCreationEvents() {
	wait.Until(c.processNextPodCreationEvent, 0, c.informerStopChan)
	log.Println("processPodCreationEvents() goroutine quitting")
}

// processNextPodCreationEvent processes the next pod watch event in the work
// queue, by sending traffic to the pods that have IP address assigned, until
// the first successful response.
func (c *TestClient) processNextPodCreationEvent() {
	item, quit := c.podCreationWorkQueue.Get()
	if quit {
		close(c.informerStopChan)
		c.podCreationWorkQueue.ShutDown()
		return
	}
	defer c.podCreationWorkQueue.Done(item)

	startTime := time.Now()

	key, ok := item.(string)
	if !ok {
		log.Printf("Failed to convert workqueue item of type %T to string key", item)
		return
	}

	// Get pod from informer's cache.
	objList, err := c.podInformer.GetIndexer().ByIndex(utils.NameIndex, key)
	if err != nil {
		log.Printf("Failed to get pod object from the informer's cache, for key %q", key)
		return
	}

	if len(objList) < 1 {
		log.Printf("Pod object does not exist in the informer's cache, for key %q", key)
		return
	}

	pod, ok := objList[0].(*corev1.Pod)
	if !ok {
		log.Printf("processNextPodCreationEvent() failed to conver obj (%T) to *corev1.Pod", objList[0])
		return
	}

	if len(pod.Status.PodIP) > 0 {
		podName := pod.GetName()
		podCreationTime := pod.GetCreationTimestamp().Time

		// Don't try to reach the pod again.
		haveMaxTargets := false
		c.seenPods.lock.Lock()
		_, ok = c.seenPods.mp[podName]
		if !ok {
			c.seenPods.mp[podName] = startTime
			haveMaxTargets = len(c.seenPods.mp) > c.Config.expectedTargets
		}
		c.seenPods.lock.Unlock()

		// Process only up to the expected number of pods.
		if haveMaxTargets {
			c.stopInformerChan()
			return
		}

		// Do the measurements only if it hasn't already been done for this pod.
		if !ok {
			target := &targetSpec{
				ip:        pod.Status.PodIP,
				name:      podName,
				namespace: pod.GetNamespace(),
				startTime: startTime,
			}
			go c.recordFirstSuccessfulRequest(target, true)

			podAssignedIpLatency := startTime.Sub(podCreationTime)
			metrics.PodIpAddressAssignedLatency.Observe(podAssignedIpLatency.Seconds())
			log.Printf("Test client got pod %q with assigned IP %q, %v after pod creation", podName, pod.Status.PodIP, podAssignedIpLatency)
		}
	}
}

// recordFirstSuccessfulRequest sends curl requests continuously to the provided
// target IP and target port from the test client config, at 1 second intervals,
// until the first successful response. It records the latency between the
// provided start time in the targetSpec, and  the time of the first successful
// response.
func (c *TestClient) recordFirstSuccessfulRequest(target *targetSpec, forPodCreation bool) {
	if target == nil || len(target.ip) == 0 {
		log.Printf("Skipping a target for policy enforcement latency (podCreation=%t), because the target does not have an IP address assigned. Target: %v", forPodCreation, target)
		return
	}

	request := fmt.Sprintf("curl --connect-timeout 1 %s:%d", target.ip, c.Config.targetPort)
	command := []string{"sh", "-c", "-x", request}
	log.Printf("Sending request %q", request)

	for {
		select {
		case <-c.MainStopChan:
			return
		default:
		}

		_, err := utils.RunCommand(command)
		if err == nil {
			if forPodCreation {
				c.reportReachedTimeForPodCreation(target, time.Now())
			} else {
				c.reportReachedTimeForPolicyCreation(target, time.Now())
			}
			return
		}
	}
}

// reportReachedTimeForPodCreation records the network policy enforcement
// latency for pod creation.
func (c *TestClient) reportReachedTimeForPodCreation(target *targetSpec, reachedTime time.Time) {
	latency := reachedTime.Sub(target.startTime)

	// Generate Prometheus metrics for latency on policy creation.
	metrics.PolicyEnforceLatencyPodCreation.Observe(latency.Seconds())
	log.Printf("Pod %q in namespace %q reached %v after pod IP was assigned", target.name, target.namespace, latency)
}

// reportReachedTimeForPolicyCreation records the network policy enforcement
// latency for network policy creation.
func (c *TestClient) reportReachedTimeForPolicyCreation(target *targetSpec, reachedTime time.Time) {
	if len(c.Config.allowPolicyName) == 0 {
		return
	}

	var policyCreateTime time.Time
	failed := false

	c.policyCreatedTime.lock.Lock()

	if c.policyCreatedTime.time == nil {
		networkPolicy, err := c.k8sClient.NetworkingV1().NetworkPolicies(c.Config.testClientNamespace).Get(context.TODO(), c.Config.allowPolicyName, metav1.GetOptions{})
		if err != nil {
			log.Printf("Failed to get network policies for pod %q in namespace %q with IP %q: %v", target.name, c.Config.testClientNamespace, target.ip, err)
			failed = true
		}

		policyCreateTime = networkPolicy.GetCreationTimestamp().Time
		c.policyCreatedTime.time = &policyCreateTime
	} else {
		policyCreateTime = *c.policyCreatedTime.time
	}

	c.policyCreatedTime.lock.Unlock()
	if failed {
		return
	}

	latency := reachedTime.Sub(policyCreateTime)

	// Generate Prometheus metrics for latency on policy creation.
	metrics.PolicyEnforceLatencyPolicyCreation.Observe(latency.Seconds())
	log.Printf("Pod %q in namespace %q with IP %q reached %v after policy creation", target.name, target.namespace, target.ip, latency)
}

// enterIdleState is used for preventing the pod from exiting after the test is
// completed, because it then gets restarted. The test client enters the idle
// state as soon as it has finished the setup and has run the test goroutines.
func (c *TestClient) enterIdleState() {
	log.Printf("Going into idle state")
	select {
	case <-c.MainStopChan:
		c.stopInformerChan()
		return
	}
}

func (c *TestClient) stopInformerChan() {
	if utils.ChannelIsOpen(c.informerStopChan) {
		close(c.informerStopChan)
	}
}
