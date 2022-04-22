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

package network_policy

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog"
	"k8s.io/perf-tests/clusterloader2/pkg/framework"
	"k8s.io/perf-tests/clusterloader2/pkg/framework/client"
	"k8s.io/perf-tests/clusterloader2/pkg/measurement"
	"k8s.io/perf-tests/clusterloader2/pkg/util"
)

/*
The measurement tests network policy enforcement latency for two cases:
	1. Created pods that are affected by network policies
Deploy the test clients (setup and start) with "podCreation" flag set to true,
before creating the target pods.
	2. Created network policies
Deploy the test clients (setup and start) after creating the target pods.

Target pods are all pods that have the specified label:
{ targetLabelKey: targetLabelValue }.
*/

const (
	networkPolicyEnforcementName = "NetworkPolicyEnforcement"
	netPolicyTestNamespace       = "net-policy-test"
	netPolicyTestClientName      = "np-test-client"
	testNamespacePrefix          = "test-"
	policyCreationPrefix         = "policy-creation"
	podCreationPrefix            = "pod-creation"

	//manifestPathPrefix             = "$GOPATH/src/k8s.io/perf-tests/clusterloader2/pkg/measurement/common/network-policy/manifests"
	// Using this during testing.
	//manifestPathPrefix             = "~/IdeaProjects/network-policy-measurement/perf-tests/clusterloader2/pkg/measurement/common/network-policy/manifests"
	manifestPathPrefix             = "./pkg/measurement/common/network-policy/manifests"
	serviceAcccountFilePath        = manifestPathPrefix + "/" + "serviceaccount.yaml"
	clusterRoleFilePath            = manifestPathPrefix + "/" + "clusterrole.yaml"
	clusterRoleBindingFilePath     = manifestPathPrefix + "/" + "clusterrolebinding.yaml"
	clientDeploymenFilePath        = manifestPathPrefix + "/" + "dep-test-client.yaml"
	policyEgressApiserverFilePath  = manifestPathPrefix + "/" + "policy-egress-allow-apiserver.yaml"
	policyEgressTargetPodsFilePath = manifestPathPrefix + "/" + "policy-egress-allow-target-pods.yaml"
)

func init() {
	klog.Infof("Registering %q", networkPolicyEnforcementName)
	if err := measurement.Register(networkPolicyEnforcementName, createNetworkPolicyEnforcementMeasurement); err != nil {
		klog.Fatalf("Cannot register %s: %v", networkPolicyEnforcementName, err)
	}
}

func createNetworkPolicyEnforcementMeasurement() measurement.Measurement {
	return &networkPolicyEnforcementMeasurement{}
}

type networkPolicyEnforcementMeasurement struct {
	k8sClient                        clientset.Interface
	framework                        *framework.Framework
	testClientNamespace              string
	targetLabelKey, targetLabelValue string
	// targetNamespaces are used to direct one client to measure a single
	// namespace.
	targetNamespaces []string
	baseline         bool
}

// Execute - Available actions:
// 1. start
// 2. complete
func (nps *networkPolicyEnforcementMeasurement) Execute(config *measurement.Config) ([]measurement.Summary, error) {
	action, err := util.GetString(config.Params, "action")
	if err != nil {
		return nil, err
	}

	switch action {
	case "setup":
		return nil, nps.setup(config)
	case "create":
		return nil, nps.create(config)
	case "gather":
		return nil, nps.gather()
	default:
		return nil, fmt.Errorf("unknown action %v", action)
	}
}

// setup initializes the measurement, creates a namespace, network policy egress
// allow to the kube-apiserver and permissions for the network policy
// enforcement test clients.
func (nps *networkPolicyEnforcementMeasurement) setup(config *measurement.Config) error {
	err := nps.initializeMeasurement(config)
	if err != nil {
		return fmt.Errorf("failed to initialize the measurement: %v", err)
	}

	if err := client.CreateNamespace(nps.k8sClient, nps.testClientNamespace); err != nil {
		return fmt.Errorf("error while creating namespace: %v", err)
	}

	return nps.createPermissionResources()
}

func (nps *networkPolicyEnforcementMeasurement) initializeMeasurement(config *measurement.Config) error {
	if nps.framework != nil {
		return fmt.Errorf("the %q is already started. Cannot start again", networkPolicyEnforcementName)
	}

	var err error
	nps.testClientNamespace, err = util.GetStringOrDefault(config.Params, "testClientNamespace", netPolicyTestNamespace)
	if err != nil {
		return err
	}

	nps.framework = config.ClusterFramework
	nps.k8sClient = config.ClusterFramework.GetClientSets().GetClient()

	namespaceList, err := nps.k8sClient.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, ns := range namespaceList.Items {
		if strings.HasPrefix(ns.GetName(), testNamespacePrefix) {
			nps.targetNamespaces = append(nps.targetNamespaces, ns.GetName())
		}
	}

	if len(nps.targetNamespaces) == 0 {
		return fmt.Errorf("cannot create the %q, no namespaces with prefix %q exist", networkPolicyEnforcementName, testNamespacePrefix)
	}

	return nil
}

// createPermissionResources creates ServiceAccount, ClusterRole and
// ClusterRoleBinding for the test client pods.
func (nps *networkPolicyEnforcementMeasurement) createPermissionResources() error {
	templateMap := map[string]interface{}{
		"Name":      netPolicyTestClientName,
		"Namespace": nps.testClientNamespace,
	}

	if err := nps.framework.ApplyTemplatedManifests(serviceAcccountFilePath, templateMap); err != nil {
		return fmt.Errorf("error while creating serviceaccount: %v", err)
	}

	if err := nps.framework.ApplyTemplatedManifests(clusterRoleFilePath, templateMap); err != nil {
		return fmt.Errorf("error while creating clusterrole: %v", err)
	}

	if err := nps.framework.ApplyTemplatedManifests(clusterRoleBindingFilePath, templateMap); err != nil {
		return fmt.Errorf("error while creating clusterrolebinding: %v", err)
	}

	return nil
}

func (nps *networkPolicyEnforcementMeasurement) create(config *measurement.Config) error {
	var err error
	nps.targetLabelKey, err = util.GetString(config.Params, "targetLabelKey")
	if err != nil {
		return err
	}

	nps.targetLabelValue, err = util.GetString(config.Params, "targetLabelValue")
	if err != nil {
		return err
	}

	metricsPort, err := util.GetIntOrDefault(config.Params, "metricsPort", 9160)
	if err != nil {
		return err
	}

	podCreation, err := util.GetBoolOrDefault(config.Params, "podCreation", false)
	if err != nil {
		return err
	}

	nps.baseline, err = util.GetBoolOrDefault(config.Params, "baseline", false)
	if err != nil {
		return err
	}

	// Baseline is only used for pod creation test. It works without creating
	// network policies.
	nps.baseline = podCreation && nps.baseline
	if !nps.baseline {
		if err = nps.createAllowApiserverPolicy(); err != nil {
			return err
		}
	}

	templateMap := map[string]interface{}{
		//"Name":                netPolicyTestClientName,
		"Namespace":           nps.testClientNamespace,
		"TestClientLabel":     netPolicyTestClientName,
		"TargetLabelSelector": fmt.Sprintf("%s = %s", nps.targetLabelKey, nps.targetLabelValue),
		"MetricsPort":         metricsPort,
		"TestPodCreation":     podCreation,
		"MeasureCilium":       podCreation,
		"ServiceAccountName":  netPolicyTestClientName,
	}

	if podCreation {
		return nps.startPodCreationTest(templateMap)
	}

	return nps.startPolicyCreationTest(templateMap)
}

func (nps *networkPolicyEnforcementMeasurement) startPodCreationTest(depTemplateMap map[string]interface{}) error {
	klog.Infof("Starting pod creation network policy enforcement latency measurement")

	if !nps.baseline {
		err := nps.createAllowPolicyToTargetPods()
		if err != nil {
			return err
		}
	}

	return nps.createTestClientDeployments(depTemplateMap, podCreationPrefix)
}

func (nps *networkPolicyEnforcementMeasurement) startPolicyCreationTest(templateMap map[string]interface{}) error {
	return nil
}

func (nps *networkPolicyEnforcementMeasurement) createAllowApiserverPolicy() error {
	policyName := "policy-allow-egress-apiserver"
	if policy, err := nps.k8sClient.NetworkingV1().NetworkPolicies(nps.testClientNamespace).Get(context.TODO(), policyName, metav1.GetOptions{}); err == nil && policy != nil {
		klog.Infof("Attempting to create %q network policy, but it already exists", policyName)
		return nil
	}

	masterIPs, err := util.GetMasterIPs(nps.k8sClient, corev1.NodeInternalIP)
	if err != nil {
		return err
	}

	if len(masterIPs) == 0 {
		return fmt.Errorf("util.GetMasterIPs(_, %q) returned 0 IPs", corev1.NodeInternalIP)
	}

	templateMap := map[string]interface{}{
		"Name":            policyName,
		"Namespace":       nps.testClientNamespace,
		"TestClientLabel": netPolicyTestClientName,
		"MasterIP":        masterIPs[0],
	}

	if err := nps.framework.ApplyTemplatedManifests(policyEgressApiserverFilePath, templateMap); err != nil {
		return fmt.Errorf("error while creating allow egress to apiserver network policy: %v", err)
	}

	return nil
}

func (nps *networkPolicyEnforcementMeasurement) createAllowPolicyToTargetPods() error {
	templateMap := map[string]interface{}{
		"Name":             "policy-allow-egress-target-pods",
		"Namespace":        nps.testClientNamespace,
		"TestClientLabel":  netPolicyTestClientName,
		"TargetLabelKey":   nps.targetLabelKey,
		"TargetLabelValue": nps.targetLabelValue,
	}

	if err := nps.framework.ApplyTemplatedManifests(policyEgressTargetPodsFilePath, templateMap); err != nil {
		return fmt.Errorf("error while creating allow egress to apiserver network policy: %v", err)
	}

	return nil
}

func (nps *networkPolicyEnforcementMeasurement) createTestClientDeployments(templateMap map[string]interface{}, namePrefix string) error {
	klog.Infof("Creating test client deployments for measurement %q", networkPolicyEnforcementName)

	// Create a test client deployment for each test namespace.
	for i, ns := range nps.targetNamespaces {
		templateMap["Name"] = fmt.Sprintf("%s-%s-%d", namePrefix, netPolicyTestClientName, i)
		templateMap["TargetNamespace"] = ns

		if err := nps.framework.ApplyTemplatedManifests(clientDeploymenFilePath, templateMap); err != nil {
			return fmt.Errorf("error while creating test client deployment: %v", err)
		}
	}

	return nil
}

func (nps *networkPolicyEnforcementMeasurement) gather() error {
	return nps.cleanUp()
}

func (nps *networkPolicyEnforcementMeasurement) deleteClusterRoleAndBinding() error {
	klog.Infof("Deleting ClusterRole and ClusterRoleBinding for measurement %q", networkPolicyEnforcementName)

	if err := nps.k8sClient.RbacV1().ClusterRoles().Delete(context.TODO(), netPolicyTestClientName, metav1.DeleteOptions{}); err != nil {
		return err
	}

	return nps.k8sClient.RbacV1().ClusterRoleBindings().Delete(context.TODO(), netPolicyTestClientName, metav1.DeleteOptions{})
}

func (nps *networkPolicyEnforcementMeasurement) cleanUp() error {
	if nps.k8sClient == nil {
		return fmt.Errorf("cleanup skipped - the measurement is not running")
	}

	if err := nps.deleteClusterRoleAndBinding(); err != nil {
		return err
	}

	klog.Infof("Deleting namespace %q for measurement %q", nps.testClientNamespace, networkPolicyEnforcementName)
	return nps.k8sClient.CoreV1().Namespaces().Delete(context.TODO(), nps.testClientNamespace, metav1.DeleteOptions{})
}

// String returns a string representation of the measurement.
func (nps *networkPolicyEnforcementMeasurement) String() string {
	return networkPolicyEnforcementName
}

// Dispose cleans up after the measurement.
func (nps *networkPolicyEnforcementMeasurement) Dispose() {
	if err := nps.cleanUp(); err != nil {
		klog.Infof("cleanUp() returns error: %v", err)
	}
}
