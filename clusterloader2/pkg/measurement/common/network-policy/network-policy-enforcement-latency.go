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
	"time"

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

	The test is set up by this measurement, by creating the required resources,
	including the network-policy-latency-client pods that are measuring the
	latencies and generating metrics for them.
*/

const (
	networkPolicyEnforcementName = "NetworkPolicyEnforcement"
	netPolicyTestNamespace       = "net-policy-test"
	netPolicyTestClientName      = "np-test-client"
	testNamespacePrefix          = "test-"
	policyCreationTest           = "policy-creation"
	podCreationTest              = "pod-creation"
	// denyLabelValue is used for network policies to allow connections only to
	// the pods with the specified label, effectively denying other connections,
	// as long as there isn't another network policy allowing it for other labels.
	denyLabelValue  = "deny-traffic"
	allowPolicyName = "allow-egress-to-target"
	denyPolicyName  = "deny-egress-to-target"

	manifestPathPrefix             = "./pkg/measurement/common/network-policy/manifests"
	serviceAccountFilePath         = manifestPathPrefix + "/" + "serviceaccount.yaml"
	clusterRoleFilePath            = manifestPathPrefix + "/" + "clusterrole.yaml"
	clusterRoleBindingFilePath     = manifestPathPrefix + "/" + "clusterrolebinding.yaml"
	clientDeploymentFilePath       = manifestPathPrefix + "/" + "dep-test-client.yaml"
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
	k8sClient           clientset.Interface
	framework           *framework.Framework
	testClientNamespace string
	targetLabelKey      string
	targetLabelValue    string
	// targetNamespaces are used to direct one client to measure a single
	// namespace.
	targetNamespaces []string
	// baseline test does not create network policies.
	// baseline is only used for pod creation latency test.
	baseline bool
}

// Execute - Available actions:
// 1. setup
// 2. create
// 3. gather
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

	// Create network policies for non-baseline test.
	if !nps.baseline {
		if err = nps.createPolicyAllowAPIServer(); err != nil {
			return err
		}

		// Create a policy that allows egress from pod creation test client pods to
		// target pods.
		if err = nps.createPolicyToTargetPods(podCreationTest, true, true); err != nil {
			return err
		}

		// Create a policy that denies egress from policy creation test client pods
		// to target pods.
		if err = nps.createPolicyToTargetPods(policyCreationTest, false, false); err != nil {
			return err
		}
	}

	return nps.createPermissionResources()
}

func (nps *networkPolicyEnforcementMeasurement) initializeMeasurement(config *measurement.Config) error {
	if nps.framework != nil {
		return fmt.Errorf("the %q is already started. Cannot start again", networkPolicyEnforcementName)
	}

	var err error
	if nps.targetLabelKey, err = util.GetString(config.Params, "targetLabelKey"); err != nil {
		return err
	}

	if nps.targetLabelValue, err = util.GetString(config.Params, "targetLabelValue"); err != nil {
		return err
	}

	if nps.testClientNamespace, err = util.GetStringOrDefault(config.Params, "testClientNamespace", netPolicyTestNamespace); err != nil {
		return err
	}

	if nps.baseline, err = util.GetBoolOrDefault(config.Params, "baseline", false); err != nil {
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

	if err := nps.framework.ApplyTemplatedManifests(serviceAccountFilePath, templateMap); err != nil {
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
	// Should this be mandatory? Fail if not provided.
	targetPort, err := util.GetIntOrDefault(config.Params, "targetPort", 80)
	if err != nil {
		return err
	}

	expectedTargets, err := util.GetIntOrDefault(config.Params, "expectedTargets", 1000)
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

	//measureCilium, err := util.GetBoolOrDefault(config.Params, "measureCilium", false)
	//if err != nil {
	//	return err
	//}

	templateMap := map[string]interface{}{
		//"Name":                netPolicyTestClientName,
		"Namespace":           nps.testClientNamespace,
		"TestClientLabel":     netPolicyTestClientName,
		"TargetLabelSelector": fmt.Sprintf("%s = %s", nps.targetLabelKey, nps.targetLabelValue),
		"TargetPort":          targetPort,
		"AllowPolicyName":     allowPolicyName,
		"MetricsPort":         metricsPort,
		"TestPodCreation":     podCreation,
		//"MeasureCilium":       measureCilium,
		"ServiceAccountName": netPolicyTestClientName,
		"ExpectedTargets":    expectedTargets,
	}

	if podCreation {
		return nps.startPodCreationTest(templateMap)
	}

	return nps.startPolicyCreationTest(templateMap)
}

func (nps *networkPolicyEnforcementMeasurement) startPodCreationTest(depTemplateMap map[string]interface{}) error {
	klog.Infof("Starting pod creation network policy enforcement latency measurement")
	return nps.createTestClientDeployments(depTemplateMap, podCreationTest)
}

func (nps *networkPolicyEnforcementMeasurement) startPolicyCreationTest(depTemplateMap map[string]interface{}) error {
	klog.Infof("Starting policy creation network policy enforcement latency measurement")

	if nps.baseline {
		klog.Infof("Baseline flag is specified, which is only used for pod creation test, and means that no network policies should be created. Skipping policy creation test")
		return nil
	}

	if err := nps.createTestClientDeployments(depTemplateMap, policyCreationTest); err != nil {
		return err
	}

	klog.Infof("Waiting for policy creation test client pods to be running")
	for retries := 0; retries < 5; retries++ {
		clientPodList, err := nps.k8sClient.CoreV1().Pods(nps.testClientNamespace).List(context.TODO(), metav1.ListOptions{LabelSelector: fmt.Sprintf("type = %s", policyCreationTest)})
		if err == nil {
			allReady := true

			for _, clientPod := range clientPodList.Items {
				if clientPod.Status.Phase != corev1.PodRunning {
					allReady = false
					break
				}
			}

			if allReady {
				klog.Infof("All policy creation test client pods are running")
				break
			}
		}

		time.Sleep(10 * time.Second)
	}

	// Create a policy that allows egress from policy creation test client pods to
	// target pods.
	return nps.createPolicyToTargetPods(policyCreationTest, false, true)
}

func (nps *networkPolicyEnforcementMeasurement) createPolicyAllowAPIServer() error {
	policyName := "allow-egress-apiserver"
	if policy, err := nps.k8sClient.NetworkingV1().NetworkPolicies(nps.testClientNamespace).Get(context.TODO(), policyName, metav1.GetOptions{}); err == nil && policy != nil {
		klog.Infof("Attempting to create %q network policy, but it already exists", policyName)
		return nil
	}

	// Get kube-apiserver IP address to allow connections to it from the test
	// client pods. It's needed since network policies are denying connections
	// with all endpoints that are not in the specified Labels / CIDR range.
	endpoints, err := nps.k8sClient.CoreV1().Endpoints(corev1.NamespaceDefault).Get(context.TODO(), "kubernetes", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get kube-apiserver Endpoints object: %v", err)
	}

	if len(endpoints.Subsets) == 0 || len(endpoints.Subsets[0].Addresses) == 0 {
		return fmt.Errorf("kube-apiserver Endpoints object does not have an IP address")
	}

	kubeAPIServerIP := endpoints.Subsets[0].Addresses[0].IP
	templateMap := map[string]interface{}{
		"Name":            policyName,
		"Namespace":       nps.testClientNamespace,
		"TestClientLabel": netPolicyTestClientName,
		"kubeAPIServerIP": kubeAPIServerIP,
	}

	if err := nps.framework.ApplyTemplatedManifests(policyEgressApiserverFilePath, templateMap); err != nil {
		return fmt.Errorf("error while creating allow egress to apiserver network policy: %v", err)
	}

	return nil
}

func (nps *networkPolicyEnforcementMeasurement) createPolicyToTargetPods(testType string, podCreation, allowForTargetPods bool) error {
	//var policyName string
	//if policy, err := nps.k8sClient.NetworkingV1().NetworkPolicies(nps.testClientNamespace).Get(context.TODO(), policyName, metav1.GetOptions{}); err == nil && policy != nil {
	//	klog.Infof("Attempting to create %q network policy, but it already exists", policyName)
	//	return nil
	//}

	templateMap := map[string]interface{}{
		//"Name":           allowPolicyName,
		"Namespace":      nps.testClientNamespace,
		"TypeLabelValue": testType,
		"TargetLabelKey": nps.targetLabelKey,
		//"TargetLabelValue": nps.targetLabelValue,
	}

	if podCreation {
		templateMap["Name"] = fmt.Sprintf("%s-%s", allowPolicyName, testType)
	} else {
		if allowForTargetPods {
			templateMap["Name"] = allowPolicyName
		} else {
			templateMap["Name"] = denyPolicyName
		}
	}

	if allowForTargetPods {
		templateMap["TargetLabelValue"] = nps.targetLabelValue
	} else {
		templateMap["TargetLabelValue"] = denyLabelValue
	}

	if err := nps.framework.ApplyTemplatedManifests(policyEgressTargetPodsFilePath, templateMap); err != nil {
		return fmt.Errorf("error while creating allow egress to pods network policy: %v", err)
	}

	return nil
}

func (nps *networkPolicyEnforcementMeasurement) createTestClientDeployments(templateMap map[string]interface{}, testType string) error {
	klog.Infof("Creating test client deployments for measurement %q", networkPolicyEnforcementName)
	templateMap["TypeLabelValue"] = testType

	// Create a test client deployment for each test namespace.
	for i, ns := range nps.targetNamespaces {
		templateMap["Name"] = fmt.Sprintf("%s-%s-%d", testType, netPolicyTestClientName, i)
		templateMap["TargetNamespace"] = ns

		// Metrics ports need to be different when there will be multiple test
		// client pods scheduled on the same node.
		//if incrementPorts {
		//	templateMap["MetricsPort"] = templateMap["MetricsPort"].(int) + 1
		//}

		if err := nps.framework.ApplyTemplatedManifests(clientDeploymentFilePath, templateMap); err != nil {
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
