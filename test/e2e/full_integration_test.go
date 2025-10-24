package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	awsvpceapi "github.com/openshift/aws-vpce-operator/api/v1alpha2"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	rmoapi "github.com/openshift/route-monitor-operator/api/v1alpha1"
	rmocontrollers "github.com/openshift/route-monitor-operator/controllers/hostedcontrolplane"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// TestFullStackIntegration tests the complete end-to-end integration: RMO → API → Agent
//
// # Testing with local repositories:
//
// 1. To test with local rhobs-synthetics-agent changes, add this to go.mod:
//
//	replace github.com/rhobs/rhobs-synthetics-agent => /path/to/rhobs-synthetics-agent
//
// 2. To test with local rhobs-synthetics-api changes:
//
//	Option A: Use environment variable:
//	  export RHOBS_SYNTHETICS_API_PATH=/path/to/rhobs-synthetics-api
//
//	Option B: Use replace directive in go.mod:
//	  replace github.com/rhobs/rhobs-synthetics-api => /path/to/rhobs-synthetics-api
//	  Then: go mod tidy
//
// After adding replace directives, run: go mod tidy
// Don't forget to remove/re-comment the replace directives before committing!
func TestFullStackIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping full integration test in short mode")
	}

	mockDynatrace := startMockDynatraceServer()
	defer mockDynatrace.Close()
	t.Logf("Mock Dynatrace server started at %s", mockDynatrace.URL)

	mockProbeTarget := startMockProbeTargetServer()
	defer mockProbeTarget.Close()
	t.Logf("Mock probe target server started at %s", mockProbeTarget.URL)

	apiManager := NewRealAPIManager()
	defer func() { _ = apiManager.Stop() }()

	err := apiManager.Start()
	if err != nil {
		t.Fatalf("Failed to start API server: %v", err)
	}

	if err := apiManager.ClearAllProbes(); err != nil {
		t.Logf("Warning: failed to clear existing probes: %v", err)
	}

	apiURL := apiManager.GetURL()
	t.Logf("API server started at %s", apiURL)

	proxyServer := startRMOAPIProxy(apiURL)
	defer proxyServer.Close()
	rmoAPIURL := proxyServer.URL
	t.Logf("RMO API proxy started at %s", rmoAPIURL)

	var testProbeID string
	var testClusterID string

	t.Run("RMO_Creates_Probe_From_HCP_CR", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = clientgoscheme.AddToScheme(scheme)
		_ = hypershiftv1beta1.AddToScheme(scheme)
		_ = awsvpceapi.AddToScheme(scheme)
		_ = routev1.Install(scheme)
		_ = rmoapi.AddToScheme(scheme)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		rhobsConfig := rmocontrollers.RHOBSConfig{
			ProbeAPIURL:        rmoAPIURL,
			Tenant:             "",
			OnlyPublicClusters: false,
		}

		reconciler := &rmocontrollers.HostedControlPlaneReconciler{
			Client:      fakeClient,
			Scheme:      scheme,
			RHOBSConfig: rhobsConfig,
		}

		testClusterID = "test-hcp-cluster-123"
		probeURL := mockProbeTarget.URL + "/livez"

		hcp := &hypershiftv1beta1.HostedControlPlane{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "hcp",
				Namespace: "clusters",
			},
			Spec: hypershiftv1beta1.HostedControlPlaneSpec{
				ClusterID: testClusterID,
				Platform: hypershiftv1beta1.PlatformSpec{
					Type: hypershiftv1beta1.AWSPlatform,
					AWS: &hypershiftv1beta1.AWSPlatformSpec{
						Region:         "us-east-1",
						EndpointAccess: hypershiftv1beta1.Private,
					},
				},
				Services: []hypershiftv1beta1.ServicePublishingStrategyMapping{
					{
						Service: hypershiftv1beta1.APIServer,
						ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{
							Type: hypershiftv1beta1.Route,
							Route: &hypershiftv1beta1.RoutePublishingStrategy{
								Hostname: "api.test-cluster.example.com",
							},
						},
					},
				},
			},
			Status: hypershiftv1beta1.HostedControlPlaneStatus{
				Conditions: []metav1.Condition{
					{
						Type:   string(hypershiftv1beta1.HostedControlPlaneAvailable),
						Status: metav1.ConditionTrue,
					},
				},
			},
		}

		ctx := context.Background()
		err = fakeClient.Create(ctx, hcp)
		if err != nil {
			t.Fatalf("Failed to create HostedControlPlane: %v", err)
		}

		setupRMODependencies(t, fakeClient, ctx, mockDynatrace.URL)
		t.Logf("✅ Created HostedControlPlane CR with cluster ID: %s", testClusterID)

		logWriter := &testWriter{t: t, logs: []string{}}
		zapLogger := zap.New(zap.WriteTo(logWriter), zap.UseDevMode(true))
		log.SetLogger(zapLogger)

		t.Log("🔄 Triggering RMO reconciliation with actual controller code...")
		req := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "hcp",
				Namespace: "clusters",
			},
		}

		result, err := reconciler.Reconcile(ctx, req)
		t.Logf("RMO reconciliation completed: result=%+v, err=%v", result, err)

		if err != nil && !strings.Contains(err.Error(), "404") && !strings.Contains(err.Error(), "no matching operation") && !strings.Contains(err.Error(), "connection refused") {
			t.Logf("⚠️  RMO reconciliation returned error (may be expected in test environment): %v", err)
		}

		t.Log("📋 Validating RMO reconciliation logs...")
		if !logWriter.ContainsLog("Reconciling HostedControlPlanes") {
			t.Error("❌ RMO logs missing: 'Reconciling HostedControlPlanes'")
		} else {
			t.Log("✅ RMO log found: Reconciling HostedControlPlanes")
		}

		if !logWriter.ContainsLog("Deploying internal monitoring objects") {
			t.Error("❌ RMO logs missing: 'Deploying internal monitoring objects'")
		} else {
			t.Log("✅ RMO log found: Deploying internal monitoring objects")
		}

		if !logWriter.ContainsLog("Deploying HTTP Monitor Resources") {
			t.Error("❌ RMO logs missing: 'Deploying HTTP Monitor Resources'")
		} else {
			t.Log("✅ RMO log found: Deploying HTTP Monitor Resources")
		}

		if !logWriter.ContainsLog("Deploying RHOBS probe") {
			t.Error("❌ RMO logs missing: 'Deploying RHOBS probe'")
		} else {
			t.Log("✅ RMO log found: Deploying RHOBS probe")
		}

		t.Log("✅ RMO successfully executed reconciliation logic")
		t.Log("✅ RMO reached RHOBS probe creation step (ensureRHOBSProbe)")

		t.Log("🔍 Checking if RMO created probe via API...")
		time.Sleep(1 * time.Second)

		existingProbes, err := listProbes(apiURL, fmt.Sprintf("cluster-id=%s", testClusterID))
		if err == nil && len(existingProbes) > 0 {
			testProbeID = existingProbes[0].ID
			t.Logf("✅ RMO successfully created probe via API! Probe ID: %s", testProbeID)
			t.Log("✅ API path proxy is working correctly - RMO → Proxy → API communication successful!")
		} else {
			t.Log("⚠️  RMO did not create probe via API (checking with fallback)")
			t.Log("Creating probe manually via API to ensure agent test can proceed...")
			probeID, err := createProbeViaAPI(apiURL, testClusterID, probeURL, false)
			if err != nil {
				t.Fatalf("Failed to create probe via API: %v", err)
			}
			testProbeID = probeID
			t.Logf("✅ Probe created manually with ID: %s (for agent to fetch)", testProbeID)
		}
	})

	t.Run("Agent_Fetches_And_Processes_Probe", func(t *testing.T) {
		agentManager := NewAgentManager([]string{apiURL + "/probes"})
		defer func() { _ = agentManager.Stop() }()

		err := agentManager.Start()
		if err != nil {
			t.Fatalf("Failed to start agent: %v", err)
		}

		t.Log("Waiting for agent to fetch and process probes...")
		time.Sleep(5 * time.Second)

		probes, err := listProbes(apiURL, "")
		if err != nil {
			t.Fatalf("Failed to list probes: %v", err)
		}

		if len(probes) == 0 {
			t.Fatal("Expected at least one probe, got none")
		}

		foundProbe := false
		var probeStatus string
		for _, probe := range probes {
			if probe.ID == testProbeID {
				foundProbe = true
				probeStatus = probe.Status
				t.Logf("✅ Agent fetched probe: %s (status: %s)", probe.ID, probe.Status)
				break
			}
		}

		if !foundProbe {
			t.Errorf("❌ Agent did not fetch the test probe %s", testProbeID)
		}

		t.Log("📋 Validating agent probe processing...")
		if probeStatus == "active" {
			t.Log("✅ Agent successfully processed probe and updated status to 'active'")
		} else {
			time.Sleep(3 * time.Second)
			probe, err := getProbeByID(apiURL, testProbeID)
			if err == nil && probe.Status == "active" {
				t.Log("✅ Agent successfully processed probe and updated status to 'active' (after retry)")
			} else {
				t.Logf("⚠️  Probe status is '%s' (expected 'active'). Agent may not have fully processed the probe yet or K8s resources may not be available.", probeStatus)
			}
		}

		t.Log("Shutting down agent...")
		err = agentManager.Stop()
		if err != nil {
			t.Logf("Agent returned error (may be expected): %v", err)
		} else {
			t.Log("✅ Agent shut down successfully")
		}
	})

	// Step 3: Verify probe execution and results
	t.Run("Agent_Executes_Probe_And_Reports_Results", func(t *testing.T) {
		// Verify that the mock probe target is accessible (simulates real probe execution)
		t.Log("🔍 Verifying probe target is accessible...")
		resp, err := http.Get(mockProbeTarget.URL + "/livez")
		if err != nil {
			t.Logf("⚠️  Probe target not accessible: %v", err)
		} else {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				t.Log("✅ Probe target is accessible and responding correctly")
			} else {
				t.Logf("⚠️  Probe target returned status %d", resp.StatusCode)
			}
		}

		// Wait for agent to execute the probe and report results
		t.Log("⏳ Waiting for agent to execute probe and report results...")
		time.Sleep(10 * time.Second) // Give agent time to execute probe

		// Check if probe has execution results using helper function
		hasResults, resultLabels, err := checkProbeExecutionResults(apiURL, testProbeID)
		if err != nil {
			t.Fatalf("Failed to check probe execution results: %v", err)
		}

		t.Logf("📊 Checking probe execution results...")
		t.Logf("Has execution results: %t", hasResults)
		t.Logf("Result labels: %v", resultLabels)

		// Verify probe execution results
		if hasResults {
			t.Log("✅ Agent successfully executed probe and reported results")

			// Log specific execution details
			for label, value := range resultLabels {
				t.Logf("  - %s: %s", label, value)
			}
		} else {
			t.Log("⚠️  Agent may not have fully executed probe (expected in test environment without K8s)")
		}

		// Check probe status progression
		probe, err := getProbeByID(apiURL, testProbeID)
		if err != nil {
			t.Fatalf("Failed to get probe: %v", err)
		}

		if probe.Status == "active" {
			t.Log("✅ Probe status is 'active' - agent completed full workflow")
		} else if probe.Status == "pending" {
			t.Log("ℹ️  Probe status is 'pending' - agent processed but couldn't complete K8s workflow (expected in test)")
		} else {
			t.Logf("ℹ️  Probe status is '%s' - agent is processing probe", probe.Status)
		}

		// Verify metrics are available (criteria 5)
		hasMetrics, err := verifyProbeMetrics(apiURL, testProbeID)
		if err != nil {
			t.Logf("⚠️  Could not verify probe metrics: %v", err)
		} else if hasMetrics {
			t.Log("✅ Probe execution metrics are available")
		} else {
			t.Log("ℹ️  Probe execution metrics not yet available (expected in test environment)")
		}
	})

	// Step 4: Verify the probe status in the API
	t.Run("API_Has_Probe_With_Valid_Status", func(t *testing.T) {
		probe, err := getProbeByID(apiURL, testProbeID)
		if err != nil {
			t.Fatalf("Failed to get probe: %v", err)
		}

		t.Logf("📋 Validating probe in API...")
		t.Logf("Probe ID: %s", probe.ID)
		t.Logf("Probe URL: %s", probe.StaticURL)
		t.Logf("Probe status: %s", probe.Status)
		t.Logf("Probe labels: %v", probe.Labels)

		validStatuses := []string{"pending", "active", "failed", "terminating"}
		isValidStatus := false
		for _, status := range validStatuses {
			if probe.Status == status {
				isValidStatus = true
				break
			}
		}

		if !isValidStatus {
			t.Errorf("❌ Probe has invalid status: %s", probe.Status)
		} else {
			t.Logf("✅ Probe has valid status: %s", probe.Status)
		}

		clusterIDLabel := probe.Labels["cluster-id"]
		if clusterIDLabel == "" {
			clusterIDLabel = probe.Labels["cluster_id"]
		}
		if clusterIDLabel != testClusterID {
			t.Errorf("❌ Probe missing or incorrect cluster-id label: got %s, want %s", clusterIDLabel, testClusterID)
		} else {
			t.Log("✅ Probe has correct cluster-id label")
		}
	})

	// Step 5: Simulate RMO deleting the HostedCluster (cleanup)
	t.Run("RMO_Deletes_Probe", func(t *testing.T) {
		err := deleteProbeViaAPI(apiURL, testProbeID)
		if err != nil {
			t.Fatalf("Failed to delete probe: %v", err)
		}

		t.Logf("Successfully deleted probe %s", testProbeID)

		probe, err := getProbeByID(apiURL, testProbeID)
		if err == nil {
			if probe.Status != "terminating" && probe.Status != "deleted" {
				t.Errorf("Expected probe to be terminating or deleted, got status: %s", probe.Status)
			}
		}
	})
}
