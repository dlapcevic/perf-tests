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
	"sync"
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
	policyLoadFilePath             = manifestPathPrefix + "/" + "policy-load.yaml"

	defaultPolicyTargetLoadBaseName = "small-deployment"
	defaultPolicyLoadCount          = 1000
	defaultPolicyLoadQPS            = 10
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
// 2. run
// 3. complete
func (nps *networkPolicyEnforcementMeasurement) Execute(config *measurement.Config) ([]measurement.Summary, error) {
	action, err := util.GetString(config.Params, "action")
	if err != nil {
		return nil, err
	}

	switch action {
	case "setup":
		return nil, nps.setup(config)
	case "run":
		return nil, nps.run(config)
	case "complete":
		return nil, nps.complete(config)
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
		if err = nps.createPolicyToTargetPods(podCreationTest, "", true, true, 0); err != nil {
			return err
		}

		// Create a policy that denies egress from policy creation test client pods
		// to target pods.
		if err = nps.createPolicyToTargetPods(policyCreationTest, "", false, false, 0); err != nil {
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

func (nps *networkPolicyEnforcementMeasurement) run(config *measurement.Config) error {
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

	templateMap := map[string]interface{}{
		//"Name":                netPolicyTestClientName,
		"Namespace":           nps.testClientNamespace,
		"TestClientLabel":     netPolicyTestClientName,
		"TargetLabelSelector": fmt.Sprintf("%s = %s", nps.targetLabelKey, nps.targetLabelValue),
		"TargetPort":          targetPort,
		"MetricsPort":         metricsPort,
		"TestPodCreation":     podCreation,
		"ServiceAccountName":  netPolicyTestClientName,
		"ExpectedTargets":     expectedTargets,
	}

	if podCreation {
		return nps.startPodCreationTest(templateMap)
	}

	return nps.startPolicyCreationTest(templateMap, config)
}

func (nps *networkPolicyEnforcementMeasurement) startPodCreationTest(depTemplateMap map[string]interface{}) error {
	klog.Infof("Starting pod creation network policy enforcement latency measurement")
	return nps.createTestClientDeployments(depTemplateMap, podCreationTest)
}

func (nps *networkPolicyEnforcementMeasurement) startPolicyCreationTest(depTemplateMap map[string]interface{}, config *measurement.Config) error {
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

	wg := sync.WaitGroup{}
	wg.Add(1)
	// Create load policies while allow policies are being created to take network
	// policy churn into account.
	go func() {
		nps.createLoadPolicies(config)
		wg.Done()
	}()

	nps.createAllowPoliciesForPolicyCreationLatency()
	wg.Wait()

	return nil
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

func (nps *networkPolicyEnforcementMeasurement) createPolicyToTargetPods(testType, targetNamespace string, podCreation, allowForTargetPods bool, idx int) error {
	templateMap := map[string]interface{}{
		"Namespace":      nps.testClientNamespace,
		"TypeLabelValue": testType,
		"TargetLabelKey": nps.targetLabelKey,
	}

	if len(targetNamespace) > 0 {
		templateMap["OnlyTargetNamespace"] = true
		templateMap["TargetNamespace"] = targetNamespace
	} else {
		templateMap["OnlyTargetNamespace"] = false
	}

	var basePolicyName string
	if allowForTargetPods {
		templateMap["TargetLabelValue"] = nps.targetLabelValue
		basePolicyName = allowPolicyName
	} else {
		templateMap["TargetLabelValue"] = denyLabelValue
		basePolicyName = denyPolicyName
	}

	if podCreation {
		templateMap["Name"] = fmt.Sprintf("%s-%s", basePolicyName, testType)
	} else {
		templateMap["Name"] = fmt.Sprintf("%s-%d", basePolicyName, idx)
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
		templateMap["AllowPolicyName"] = fmt.Sprintf("%s-%d", allowPolicyName, i)

		if err := nps.framework.ApplyTemplatedManifests(clientDeploymentFilePath, templateMap); err != nil {
			return fmt.Errorf("error while creating test client deployment: %v", err)
		}
	}

	return nil
}

func (nps *networkPolicyEnforcementMeasurement) createLoadPolicies(config *measurement.Config) {
	policyLoadTargetBaseName, err := util.GetStringOrDefault(config.Params, "policyLoadTargetBaseName", defaultPolicyTargetLoadBaseName)
	if err != nil {
		klog.Errorf("Failed getting parameter policyLoadBaseName value, error: %v", err)
		return
	}

	policyLoadCount, err := util.GetIntOrDefault(config.Params, "policyLoadCount", defaultPolicyLoadCount)
	if err != nil {
		klog.Errorf("Failed getting parameter policyLoadBaseName value, error: %v", err)
		return
	}

	policyLoadQPS, err := util.GetIntOrDefault(config.Params, "policyLoadQPS", defaultPolicyLoadQPS)
	if err != nil {
		klog.Errorf("Failed getting parameter policyLoadQPS value, error: %v", err)
		return
	}

	expectedFinishTime := time.Now().Add(time.Duration(policyLoadCount/policyLoadQPS) * time.Second)
	// Should it also consider the time it takes kube-apiserver to process these
	// requests? Similar to how it waits for all deployments to be created. If the
	// QPS of requests for creating policies is too high, this time will not be
	// sufficient to guard the next steps of the test from getting affected.
	// Can the processing rate be estimated? (e.g. 100 QPS for network policy)?

	policiesPerNs := policyLoadCount / len(nps.targetNamespaces)
	qpsSleepDuration := (1 * time.Second) / time.Duration(policyLoadQPS)

	for nsIdx, ns := range nps.targetNamespaces {
		baseCidr := fmt.Sprintf("10.0.%d.0/24", nsIdx)

		for depIdx := 0; depIdx < policiesPerNs; depIdx++ {
			// This will be the same as "small-deployment-0".."small-deployment-50",
			// that is used in the load test.
			podSelectorLabelValue := fmt.Sprintf("policy-load-%s-%d", policyLoadTargetBaseName, depIdx)
			templateMapForTargetPods := map[string]interface{}{
				"Name":                  fmt.Sprintf("%s-%d", podSelectorLabelValue, nsIdx),
				"Namespace":             ns,
				"PodSelectorLabelKey":   "name",
				"PodSelectorLabelValue": podSelectorLabelValue,
				"CIDR":                  baseCidr,
			}

			go func() {
				if err := nps.framework.ApplyTemplatedManifests(policyLoadFilePath, templateMapForTargetPods); err != nil {
					klog.Errorf("error while creating load network policy for label selector 'name=%s': %v", podSelectorLabelValue, err)
				}
			}()

			time.Sleep(qpsSleepDuration)
		}
	}

	time.Sleep(time.Until(expectedFinishTime))
}

func (nps *networkPolicyEnforcementMeasurement) createAllowPoliciesForPolicyCreationLatency() {
	klog.Infof("Creating allow network policies for measurement %q", networkPolicyEnforcementName)

	for i, ns := range nps.targetNamespaces {
		err := nps.createPolicyToTargetPods(policyCreationTest, ns, false, true, i)
		if err != nil {
			klog.Errorf("Failed to create a network policy to allow traffic to namespace %q", ns)
		}
	}
}

// complete deletes test client deployments for the specified test mode.
func (nps *networkPolicyEnforcementMeasurement) complete(config *measurement.Config) error {
	podCreation, err := util.GetBoolOrDefault(config.Params, "podCreation", false)
	if err != nil {
		return err
	}

	typeLabelValue := policyCreationTest
	if podCreation {
		typeLabelValue = podCreationTest
	}

	listOpts := metav1.ListOptions{LabelSelector: fmt.Sprintf("type=%s", typeLabelValue)}
	err = nps.k8sClient.AppsV1().Deployments(nps.testClientNamespace).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, listOpts)
	if err != nil {
		return fmt.Errorf("failed to complete %q test, error: %v", typeLabelValue, err)
	}

	return nil
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
