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

	// PolicyEnforceLatencyPodCreation is measured by watching for pod creations
	// and updates and immediately sending traffic to them, as soon as IP has been
	// assigned, to get a timestamp of the first successful request.
	// Pod's creationTimestamp (Start time).
	// First successful request (End time).
	// Reported time = End time - Start time.
	PolicyEnforceLatencyPodCreation = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "policy_enforcement_latency_pod_creation_seconds",
			Help:    "Latency (in seconds) for network policies to be enforced for new pods",
			Buckets: latencyBuckets,
		},
	)
	// PolicyEnforceLatencyPolicyCreation is measured by continuously sending
	// requests to a service to get a timestamp of the first successful request.
	// Network policy's creationTimestamp (Start time).
	// First successful request (End time).
	// Reported time = End time - Start time.
	PolicyEnforceLatencyPolicyCreation = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "policy_enforcement_latency_policy_creation_seconds",
			Help:    "Latency (in seconds) for new network policies to be enforced",
			Buckets: latencyBuckets,
		},
	)
	// PodIpAddressAssignedLatency is measured by watching for pod updates.
	// Pod's creationTimestamp (Start time).
	// The first pod update that has IP assigned (End time).
	// Reported time = End time - Start time.
	PodIpAddressAssignedLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "pod_ip_address_assigned_latency_seconds",
			Help:    "Latency (in seconds) for IP address to be assigned to a pod, after pod creation",
			Buckets: latencyBuckets,
		},
	)
)

var register sync.Once

// RegisterMetrics registers Prometheus metrics to be collected, based on the
// test client configuration.
func RegisterMetrics(podCreation bool) {
	register.Do(func() {
		if podCreation {
			prometheus.MustRegister(PolicyEnforceLatencyPodCreation)
			prometheus.MustRegister(PodIpAddressAssignedLatency)
		} else {
			prometheus.MustRegister(PolicyEnforceLatencyPolicyCreation)
		}
	})
}

// StartMetricsServer runs a Prometheus HTTP server that exposes metrics on the
// specified port.
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
