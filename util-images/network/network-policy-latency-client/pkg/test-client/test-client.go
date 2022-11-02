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

package testclient

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
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/perf-tests/util-images/network/network-policy-latency-client/pkg/metrics"
	"k8s.io/perf-tests/util-images/network/network-policy-latency-client/pkg/utils"
)

type TestClient struct {
	Config        *config
	metricsServer *http.Server
	k8sClient     *clientset.Clientset
	MainStopChan  chan os.Signal

	informerStopChan chan struct{}
	maps             *maps

	podInformer       cache.SharedIndexInformer
	workQueue         *workqueue.Type
	policyCreatedTime *timeObject
}

type config struct {
	testClientNamespace string
	targetLabelSelector string
	targetNamespace     string
	testType            string
	targetPort          int
	expectedTargets     int
	metricsPort         int

	testPodCreation bool
	measureCilium   bool
	workerCount     int
}

type timeMap struct {
	mp   map[string]time.Time
	lock sync.RWMutex
}

type boolMap struct {
	mp   map[string]bool
	lock sync.RWMutex
}

type maps struct {
	seenPods      *timeMap
	readyPods     *boolMap
	createTimeCEP *timeMap
	seenCEP       *boolMap
}

type timeObject struct {
	time *time.Time
	lock sync.RWMutex
}

func CreateConfig() *config {
	config := config{}

	flag.StringVar(&config.targetLabelSelector, "targetLabelSelector", "", "The label selector for target pods")
	flag.StringVar(&config.testClientNamespace, "testClientNamespace", "net-policy-test", "The namespace of client pods")
	flag.StringVar(&config.targetNamespace, "targetNamespace", "", "The namespace of target pods")
	flag.StringVar(&config.testType, "testType", "policy-creation", "The type label value for test client pods")
	flag.IntVar(&config.targetPort, "targetPort", 80, "The port for the targeted pods")
	flag.IntVar(&config.expectedTargets, "expectedTargets", 100, "The expected number of target pods")
	flag.IntVar(&config.metricsPort, "metricsPort", 0, "The port number where Prometheus metrics are exposed")
	flag.BoolVar(&config.testPodCreation, "testPodCreation", false, "Specifies whether to test latency for pod creation")
	flag.BoolVar(&config.measureCilium, "measureCilium", false, "Specifies whether to watch and generate metrics for Cilium resources")
	flag.IntVar(&config.workerCount, "workerCount", 10, "The number of workers to process the events in the workerQueue")
	flag.Parse()

	return &config
}

func (c *TestClient) initialize() error {
	log.Printf("Starting test client! Time: %v", time.Now())

	if len(c.Config.targetLabelSelector) == 0 {
		return fmt.Errorf("targetLabelSelector is not specified")
	}

	if len(c.Config.targetNamespace) == 0 {
		return fmt.Errorf("targetNamespace is not specified")
	}

	if c.Config.metricsPort == 0 {
		return fmt.Errorf("metricsPort is not specified")
	}

	k8sClient, err := utils.NewK8sClient()
	if err != nil {
		return fmt.Errorf("failed to create k8s client, err - %v", err)
	}

	c.k8sClient = k8sClient
	c.workQueue = workqueue.New()

	return nil
}

func (c *TestClient) Run() {
	if err := c.initialize(); err != nil {
		log.Fatalf("init() failed: %v", err)
		return
	}

	c.metricsServer = metrics.StartMetricsServer(fmt.Sprintf(":%d", c.Config.metricsPort))
	metrics.RegisterMetrics(&metrics.MetricsConfig{
		EnableCiliumEndpointSliceMetrics: c.Config.measureCilium,
	})
	defer func(metricsServer *http.Server, ctx context.Context) {
		if err := metricsServer.Shutdown(ctx); err != nil {
			log.Printf("metricsServer Shutdown returned error - %v", err)
		}
	}(c.metricsServer, context.TODO())

	if c.Config.testPodCreation {
		c.startMeasurePodCreation()
	} else {
		c.startMeasureNetPolicyCreation()
	}
	// Prevents from pod restarting, after everything is finished.
	// The test will run only once, when the application is deployed.
	// Restart the application to rerun the test.
	c.enterIdleState()
}

func (c *TestClient) startMeasureNetPolicyCreation() {
	log.Println("Starting to measure network policy enforcement latency for network policy creation")

	podList, err := c.targetPodList()
	if err != nil {
		log.Printf("run() failed: %v", err)
		return
	}

	log.Printf("%d pods listed", len(podList))
	wg := sync.WaitGroup{}
	c.policyCreatedTime = &timeObject{lock: sync.RWMutex{}}

	for _, pod := range podList {
		pod := pod
		wg.Add(1)
		go func() {
			c.recordFirstSuccessfulRequest(&pod, false, time.Now())
			wg.Done()
		}()
	}

	wg.Wait()
}

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

func (c *TestClient) startMeasurePodCreation() {
	log.Println("Starting to measure network policy enforcement latency for pod creation")
	c.informerStopChan = make(chan struct{})
	c.initMaps()

	// Create work queues.
	if c.Config.workerCount < 1 {
		log.Fatal("Less than 1 worker queue specified")
	}

	for i := 0; i < c.Config.workerCount; i++ {
		go c.processPodCreationEvents()
	}

	// Create a pod watcher, for the specified namespace and labels.
	if err := c.createPodWatcher(); err != nil {
		log.Fatalf("createPodWatcher() failed: %v", err)
	}
}

func (c *TestClient) initMaps() {
	c.maps = &maps{
		seenPods:  &timeMap{mp: make(map[string]time.Time), lock: sync.RWMutex{}},
		readyPods: &boolMap{mp: make(map[string]bool), lock: sync.RWMutex{}},
	}

	if c.Config.measureCilium {
		c.maps.createTimeCEP = &timeMap{mp: make(map[string]time.Time), lock: sync.RWMutex{}}
		c.maps.seenCEP = &boolMap{mp: make(map[string]bool), lock: sync.RWMutex{}}

		if err := c.createCiliumEndpointSliceWatcher(); err != nil {
			log.Fatalf("createCiliumEndpointSliceWatcher() failed: %v", err)
		}
	}
}

func (c *TestClient) createPodWatcher() error {
	log.Printf("Creating PodWatcher for namespace %q, labelSelector %q", c.Config.targetNamespace, c.Config.targetLabelSelector)

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
			log.Printf("handleEvent() failed to convert newObj (%T) to *corev1.Pod", obj)
			return
		}

		// Why did I just put name as key?
		// I can put the entire pod object as key and use that instead of later
		// getting it by name from cache.
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

	err := utils.Retry(10, 10*time.Millisecond, func() error {
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

	podName := pod.GetName()
	podCreationTime := pod.GetCreationTimestamp().Time

	if c.Config.measureCilium {
		c.maps.createTimeCEP.lock.Lock()
		if _, ok := c.maps.createTimeCEP.mp[podName]; !ok {
			c.maps.createTimeCEP.mp[podName] = podCreationTime
		}
		c.maps.createTimeCEP.lock.Unlock()
	}

	if pod.Status.Phase == corev1.PodRunning {
		c.maps.readyPods.lock.Lock()
		if _, ok = c.maps.readyPods.mp[podName]; !ok {
			c.maps.readyPods.mp[podName] = true
			readyPodLatency := startTime.Sub(podCreationTime)
			metrics.PodReadyLatency.Observe(readyPodLatency.Seconds())
		}
		c.maps.readyPods.lock.Unlock()
	}

	if len(pod.Status.PodIP) > 0 {
		//var podCount int

		// Don't try to reach the pod again.
		c.maps.seenPods.lock.Lock()
		_, ok = c.maps.seenPods.mp[podName]
		if !ok {
			c.maps.seenPods.mp[podName] = startTime
			//podCount = len(c.maps.seenPods.mp)
		}
		c.maps.seenPods.lock.Unlock()

		// Do the measurements only if it hasn't already been done for this pod.
		if !ok {
			go c.recordFirstSuccessfulRequest(pod, true, podCreationTime)

			podAssignedIpLatency := startTime.Sub(podCreationTime)
			metrics.PodIpAddressAssignedLatency.Observe(podAssignedIpLatency.Seconds())
			log.Printf("Test client got pod %q with assigned IP %q, %v after pod creation", podName, pod.Status.PodIP, podAssignedIpLatency)

			//if podCount == c.Config.expectedTargets {
			//	// Generate pod's created to scheduled duration metrics.
			//	go c.reportCreatedToScheduledLatencies()
			//}
		}
	}
}

func (c *TestClient) recordFirstSuccessfulRequest(pod *corev1.Pod, forPodCreation bool, startTime time.Time) {
	request := fmt.Sprintf("curl --connect-timeout 1 %s:%d", pod.Status.PodIP, c.Config.targetPort)
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
				c.reportReachedTimeForPodCreation(pod, time.Now(), startTime)
			} else {
				c.reportReachedTimeForPolicyCreation(pod, time.Now())
			}
			return
		}
	}
}

func (c *TestClient) reportReachedTimeForPodCreation(pod *corev1.Pod, reachedTime, startTime time.Time) {
	//podCreationTime := pod.GetCreationTimestamp().Time
	//latency := reachedTime.Sub(podCreationTime)

	latency := reachedTime.Sub(startTime)

	// Generate Prometheus metrics for latency on policy creation.
	metrics.PolicyEnforceLatencyPodCreation.Observe(latency.Seconds())
	log.Printf("Pod %q in namespace %q reached %v after pod IP was assigned", pod.GetName(), pod.GetNamespace(), latency)
}

func (c *TestClient) reportReachedTimeForPolicyCreation(pod *corev1.Pod, reachedTime time.Time) {
	var policyCreateTime time.Time
	failed := false

	c.policyCreatedTime.lock.Lock()

	if c.policyCreatedTime.time == nil {
		labelSelector := fmt.Sprintf("%s = %s", utils.MatchNetPolicyByTypeLabelKey, c.Config.testType)

		// Get network policy.
		netPolicyList, err := c.k8sClient.NetworkingV1().NetworkPolicies(c.Config.testClientNamespace).List(context.TODO(), metav1.ListOptions{LabelSelector: labelSelector, Limit: 1})
		if err != nil {
			log.Printf("Failed to list network policies for pod %q in namespace %q: %v", pod.GetName(), c.Config.testClientNamespace, err)
			failed = true
		}

		if len(netPolicyList.Items) == 0 {
			log.Printf("No network policies found in namespace %q for label selector %q", c.Config.testClientNamespace, labelSelector)
			failed = true
		}

		policyCreateTime = netPolicyList.Items[0].GetCreationTimestamp().Time
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
	log.Printf("Pod %q in namespace %q reached %v after policy creation", pod.GetName(), pod.GetNamespace(), latency)
}

func (c *TestClient) reportCreatedToScheduledLatencies() {
	selector := fields.Set{
		"involvedObject.kind": "Pod",
		"source":              corev1.DefaultSchedulerName,
	}.AsSelector().String()

	options := metav1.ListOptions{FieldSelector: selector, Limit: int64(c.Config.expectedTargets)}
	scheduledEvents, err := c.k8sClient.CoreV1().Events(c.Config.targetNamespace).List(context.TODO(), options)
	if err != nil {
		log.Printf("Failed to list scheduler events for pods in namespace %q", c.Config.targetNamespace)
		return
	}

	for _, event := range scheduledEvents.Items {
		c.podCreatedToScheduledLatency(&event)
	}
}

func (c *TestClient) podCreatedToScheduledLatency(event *corev1.Event) {
	key := event.InvolvedObject.Name

	c.maps.seenPods.lock.RLock()
	createTime, exists := c.maps.seenPods.mp[key]
	c.maps.seenPods.lock.RUnlock()

	if exists {
		var latency time.Duration

		if !event.EventTime.IsZero() {
			//latency = event.EventTime.Time.Sub(createTime)
			latency = createTime.Sub(event.EventTime.Time)
		} else {
			//latency = event.FirstTimestamp.Time.Sub(createTime)
			latency = createTime.Sub(event.FirstTimestamp.Time)
		}

		metrics.PodScheduledLatency.Observe(latency.Seconds())
	}
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
	if c.Config.testPodCreation {
		close(c.informerStopChan)
	}
}
