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

package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	client "k8s.io/perf-tests/network/tools/connectivity-client/pkg"
)

func main() {
	log.Println("Starting in-cluster network connectivity test client")
	defer log.Println("Closing in-cluster network connectivity test client")

	config := client.CreateConfig()
	testClient := client.NewTestClient(config, make(chan os.Signal, 1))

	signal.Notify(testClient.MainStopChan, syscall.SIGTERM)
	testClient.Run()
}
