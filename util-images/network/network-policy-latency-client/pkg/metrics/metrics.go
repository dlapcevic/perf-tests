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

package metrics

import (
	"log"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	latencyBuckets = []float64{0.1, 0.5, 1, 3, 5, 10, 30, 60, 300, 600, 1800, 3600}

	PolicyEnforceLatencyPodCreation = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "policy_enforcement_latency_pod_creation_seconds",
			Help:    "Latency (in seconds) for network policies to be enforced for new pods",
			Buckets: latencyBuckets,
		},
	)
	PolicyEnforceLatencyPolicyCreation = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "policy_enforcement_latency_policy_creation_seconds",
			Help:    "Latency (in seconds) for new network policies to be enforced",
			Buckets: latencyBuckets,
		},
	)
	// Time taken is measured by watching for pod updates.
	// Pod's creationTimestamp (Start time).
	// The first pod update that has IP assigned (End time).
	// Reported time = End time - Start time.
	PodIpAddressAssignedLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "pod_ip_address_assigned_latency_seconds",
			Help:    "Latency (in seconds) for IP address to be assigned to a pod, after pod's creation",
			Buckets: latencyBuckets,
		},
	)
	PodReadyLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "pod_ready_latency_seconds",
			Help:    "Latency (in seconds) for a pod to be in the ready state after pod's creation",
			Buckets: latencyBuckets,
		},
	)
	PodScheduledLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "pod_scheduled_latency_seconds",
			Help:    "Latency (in seconds) for a pod to be in the ready state after being scheduled",
			Buckets: latencyBuckets,
		},
	)
	CEPPropagationLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "cep_propagation_latency_seconds",
			Help:    "Latency (in seconds) for CiliumEndpoint to reach a node through CiliumEndpointSlice",
			Buckets: latencyBuckets,
		},
	)
)

type MetricsConfig struct {
	EnableCiliumEndpointSliceMetrics bool
}

var register sync.Once

func RegisterMetrics(config *MetricsConfig) {
	register.Do(func() {
		prometheus.MustRegister(PolicyEnforceLatencyPodCreation)
		prometheus.MustRegister(PolicyEnforceLatencyPolicyCreation)
		prometheus.MustRegister(PodIpAddressAssignedLatency)
		prometheus.MustRegister(PodReadyLatency)
		prometheus.MustRegister(PodScheduledLatency)

		if config.EnableCiliumEndpointSliceMetrics {
			prometheus.MustRegister(CEPPropagationLatency)
		}
	})
}

func StartMetricsServer(listenAddr string) *http.Server {
	http.Handle("/metrics", promhttp.Handler())
	http.Handle("/healthz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	server := &http.Server{Addr: listenAddr}
	go func(server *http.Server) {
		log.Printf("Starting HTTP server on %q.", listenAddr)
		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("Metrics server failed to start, err - %v", err)
		}
	}(server)
	return server
}
