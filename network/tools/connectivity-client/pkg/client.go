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
	informerStopChan chan struct{}
	podInformer      cache.SharedIndexInformer
	workQueue        *workqueue.Type
	seenPods         *timeMapWithLock
}

type config struct {
	testClientNamespace string
	targetLabelSelector string
	targetNamespace     string
	targetPort          int
	expectedTargets     int
	allowPolicyName     string
	testTypeName        string
	metricsPort         int

	testPodCreation bool
	workerCount     int
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

func CreateConfig() *config {
	config := config{}

	flag.StringVar(&config.testClientNamespace, "testClientNamespace", "", "The namespace of client pods")
	flag.StringVar(&config.targetLabelSelector, "targetLabelSelector", "", "The label selector for target pods")
	flag.StringVar(&config.targetNamespace, "targetNamespace", "", "The namespace of target pods")
	flag.IntVar(&config.targetPort, "targetPort", 0, "The port for the targeted pods")
	flag.IntVar(&config.expectedTargets, "expectedTargets", 100, "The expected number of target pods")
	flag.StringVar(&config.allowPolicyName, "allowPolicyName", "", "The name of the policy that allows traffic to target pods.")
	flag.IntVar(&config.metricsPort, "metricsPort", 9154, "The port number where Prometheus metrics are exposed")
	flag.BoolVar(&config.testPodCreation, "testPodCreation", false, "Specifies whether to test latency for pod creation")
	flag.IntVar(&config.workerCount, "workerCount", 10, "The number of workers to process the events in the workerQueue")
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
	c.workQueue = workqueue.New()

	return nil
}

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

	if c.Config.workerCount < 1 {
		return fmt.Errorf("less than 1 worker queue specified")
	}

	if c.Config.testPodCreation {
		c.Config.testTypeName = "pod-creation"
	} else {
		if len(c.Config.allowPolicyName) == 0 {
			fmt.Println("allowPolicyName is not specified for policy creation test")
			c.Config.testTypeName = "service-reachability"
		} else {
			c.Config.testTypeName = "policy-creation"
		}
	}

	fmt.Printf("Connectivity test client mode: %q", c.Config.testTypeName)

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

func (c *TestClient) startMeasureNetPolicyCreation() error {
	log.Println("Starting to measure service reachability latency after allow policy creation")

	serviceList, err := c.targetServiceList()
	if err != nil {
		//log.Printf("Failed to list services: %v\n", err)
		return fmt.Errorf("failed to list services, error: %v\n", err)
	}

	log.Printf("%d services listed\n", len(serviceList))
	metrics.TargetServicesCount.Add(float64(len(serviceList)))

	wg := sync.WaitGroup{}
	c.policyCreatedTime = &timeWithLock{lock: sync.RWMutex{}}

	// Keep sending requests to all services until all of them are reached.
	for _, service := range serviceList {
		target := &targetSpec{
			ip:        service.Spec.ClusterIP,
			name:      service.GetName(),
			namespace: service.GetNamespace(),
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

func (c *TestClient) targetServiceList() ([]corev1.Service, error) {
	serviceList, err := c.k8sClient.CoreV1().Services(c.Config.targetNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: c.Config.targetLabelSelector, Limit: int64(c.Config.expectedTargets)})
	if err != nil {
		return nil, fmt.Errorf("failed to list services for selector %q in namespace %q: %v", c.Config.targetLabelSelector, c.Config.targetNamespace, err)
	}

	if len(serviceList.Items) == 0 {
		return nil, fmt.Errorf("no services listed for selector %q in namespace %q: %v", c.Config.targetLabelSelector, c.Config.targetNamespace, err)
	}

	return serviceList.Items, nil
}

func (c *TestClient) startMeasurePodCreation() error {
	log.Println("Starting to measure pod creation reachability latency")
	c.informerStopChan = make(chan struct{})
	c.seenPods = &timeMapWithLock{mp: make(map[string]time.Time), lock: sync.RWMutex{}}

	// Run workers.
	log.Printf("Starting %d workers for parallel processing of pod add and update events\n", c.Config.workerCount)
	for i := 0; i < c.Config.workerCount; i++ {
		go c.processPodCreationEvents()
	}

	return c.createPodWatcher()
}

func (c *TestClient) createPodWatcher() error {
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

		c.workQueue.Add(pod.GetName())
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

func (c *TestClient) processPodCreationEvents() {
	wait.Until(c.processNextPodCreationEvent, 0, c.informerStopChan)
	log.Println("processPodCreationEvents() goroutine quitting")
}

func (c *TestClient) processNextPodCreationEvent() {
	item, quit := c.workQueue.Get()
	if quit {
		close(c.informerStopChan)
		c.workQueue.ShutDown()
		return
	}
	defer c.workQueue.Done(item)

	startTime := time.Now()

	key, ok := item.(string)
	if !ok {
		log.Printf("Failed to convert workqueue item (%T) to string key", item)
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
		maxTargetsReached := false
		c.seenPods.lock.Lock()
		_, ok = c.seenPods.mp[podName]
		if !ok {
			c.seenPods.mp[podName] = startTime
			maxTargetsReached = len(c.seenPods.mp) > c.Config.expectedTargets
		}
		c.seenPods.lock.Unlock()

		// Process only up to the expected number of pods.
		if maxTargetsReached {
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
				//startTime: podCreationTime,
			}
			go c.recordFirstSuccessfulRequest(target, true)

			podAssignedIpLatency := startTime.Sub(podCreationTime)
			metrics.PodIpAddressAssignedLatency.Observe(podAssignedIpLatency.Seconds())
			log.Printf("Test client got pod %q with assigned IP %q, %v after pod creation", podName, pod.Status.PodIP, podAssignedIpLatency)
		}
	}
}

func (c *TestClient) recordFirstSuccessfulRequest(target *targetSpec, forPodCreation bool) {
	request := fmt.Sprintf("curl --connect-timeout 1 %s:%d", target.ip, c.Config.targetPort)
	command := []string{"sh", "-c", "-x", request}
	log.Printf("Sending request %q", request)

	for {
		select {
		case <-c.MainStopChan:
			c.stopInformerChan()
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

func (c *TestClient) reportReachedTimeForPodCreation(target *targetSpec, reachedTime time.Time) {
	//podCreationTime := pod.GetCreationTimestamp().Time
	//latency := reachedTime.Sub(podCreationTime)

	latency := reachedTime.Sub(target.startTime)

	// Generate Prometheus metrics for latency on policy creation.
	metrics.PolicyEnforceLatencyPodCreation.Observe(latency.Seconds())
	log.Printf("Pod %q in namespace %q reached %v after pod IP was assigned", target.name, target.namespace, latency)
}

func (c *TestClient) reportReachedTimeForPolicyCreation(target *targetSpec, reachedTime time.Time) {
	// Generate Prometheus metrics for service reachability.
	metrics.TargetServicesReachedCount.Add(1)
	if len(c.Config.allowPolicyName) == 0 {
		return
	}

	var policyCreateTime time.Time
	failed := false

	c.policyCreatedTime.lock.Lock()

	if c.policyCreatedTime.time == nil {
		networkPolicy, err := c.k8sClient.NetworkingV1().NetworkPolicies(c.Config.testClientNamespace).Get(context.TODO(), c.Config.allowPolicyName, metav1.GetOptions{})
		if err != nil {
			log.Printf("Failed to get network policies for service %q in namespace %q: %v", target.name, c.Config.testClientNamespace, err)
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
	log.Printf("Service %q in namespace %q reached %v after policy creation", target.name, target.namespace, latency)
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
	if utils.ChannelIsOpen(c.informerStopChan) {
		close(c.informerStopChan)
	}
}
