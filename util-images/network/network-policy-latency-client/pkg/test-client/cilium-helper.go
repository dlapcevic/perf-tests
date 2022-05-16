package testclient

import (
	"fmt"
	"log"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/perf-tests/util-images/network/network-policy-latency-client/api/cilium.io/v2alpha1"
	"k8s.io/perf-tests/util-images/network/network-policy-latency-client/pkg/metrics"
	"k8s.io/perf-tests/util-images/network/network-policy-latency-client/pkg/utils"
)

func (c *TestClient) createCiliumEndpointSliceWatcher() error {
	log.Printf("Creating CiliumEndpointSlice watcher for %q namespace", c.Config.targetNamespace)

	dynamicClient, err := utils.NewDynamicClient()
	if err != nil {
		return err
	}

	handleEvent := func(obj interface{}) {
		reachedTime := time.Now()

		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			log.Printf("CiliumEndpointSlice Add failed to convert obj of type %T to *unstructured.Unstructured", obj)
			return
		}

		ciliumEndpointSlice, err := ConvertUnstructuredToCiliumEndpointSlice(u)
		if err != nil {
			log.Printf("Cannot convert object %s/%s to EndpointSlice: %v", u.GetNamespace(), u.GetName(), err)
			return
		}

		if ciliumEndpointSlice.Namespace != c.Config.targetNamespace {
			log.Printf("Skipping CiliumEndpointSlice %q, because it is not in the target namespace.", ciliumEndpointSlice.Name)
			return
		}

		// Record prometheus metric for every CEP received for the first time.
		c.recordCEPPropagationLatency(ciliumEndpointSlice, reachedTime)
	}

	cesEventHandlers := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) { handleEvent(obj) },
	}

	cesInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, utils.InformerResyncTime)
	cesInformer := cesInformerFactory.ForResource(utils.CiliumEndpointSliceGVR).Informer()
	cesInformer.AddEventHandler(cesEventHandlers)

	go cesInformer.Run(c.informerStopChan)
	err = utils.Retry(10, 10*time.Millisecond, func() error {
		return utils.InformerSynced(cesInformer.HasSynced, "pod informer")
	})

	return err
}

// ConvertUnstructuredToCiliumEndpointSlice converts an unstructured object to a discovery.EndpointSlice object.
func ConvertUnstructuredToCiliumEndpointSlice(u *unstructured.Unstructured) (*v2alpha1.CiliumEndpointSlice, error) {
	if u == nil {
		return nil, nil
	}
	ciliumEndpointSlice := &v2alpha1.CiliumEndpointSlice{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, ciliumEndpointSlice); err != nil {
		return nil, fmt.Errorf("cannot convert from Unstructured to EndpointSlice: %w", err)
	}
	return ciliumEndpointSlice, nil
}

func (c *TestClient) recordCEPPropagationLatency(ces *v2alpha1.CiliumEndpointSlice, reachedTime time.Time) {
	for _, cep := range ces.Endpoints {
		c.maps.createTimeCEP.lock.RLock()
		creationTime, ok := c.maps.createTimeCEP.mp[cep.Name]
		c.maps.createTimeCEP.lock.RUnlock()

		// Pod creation was not recorded.
		if !ok {
			continue
		}

		c.maps.seenCEP.lock.Lock()
		if _, ok = c.maps.seenCEP.mp[cep.Name]; !ok {
			c.maps.seenCEP.mp[cep.Name] = true
		}
		c.maps.seenCEP.lock.Unlock()

		// Already recorded the prometheus metric data point.
		if ok {
			continue
		}

		latency := reachedTime.Sub(creationTime)
		metrics.CEPPropagationLatency.Observe(latency.Seconds())
		log.Printf("CEP %q reached through CES %v after pod creation", cep.Name, latency)
	}
}
