# Full Integration Test - RMO → API → Agent

## Overview

This directory contains a comprehensive end-to-end integration test that validates the complete workflow:

**RMO** (Route Monitor Operator) → **API** (RHOBS Synthetics API) → **Agent** (RHOBS Synthetics Agent)

The test ensures that:
1. RMO creates synthetic probes via the API when HostedControlPlane CRs are reconciled
2. The Synthetics API successfully stores and manages probe configurations
3. The Synthetics Agent fetches probes and processes them

## Test Files

### Core Test Files
- **`full_integration_test.go`** - Main test that orchestrates the full workflow
- **`full_integration_helpers.go`** - Helper functions for API interactions, mock servers, and test utilities
- **`api_manager.go`** - Manages the lifecycle of the RHOBS Synthetics API server (builds and runs as subprocess)
- **`agent_manager.go`** - Manages the lifecycle of the RHOBS Synthetics Agent (builds and runs as subprocess)

### Test Workflow

1. **Setup Phase**
   - Start mock Dynatrace server
   - Start mock probe target server (simulates cluster API endpoint)
   - Build and start RHOBS Synthetics API
   - Start API proxy to translate RMO's path format (`/api/metrics/v1/{tenant}/probes`) to API format (`/probes`)

2. **RMO Reconciliation** (`RMO_Creates_Probe_From_HCP_CR`)
   - Create a HostedControlPlane CR with cluster ID
   - Set up RMO dependencies (K8s services, VpcEndpoint, Dynatrace secret)
   - Trigger RMO reconciliation using actual controller code
   - Validate RMO logs show correct reconciliation steps
   - Verify probe was created via API

3. **Agent Processing** (`Agent_Fetches_And_Processes_Probe`)
   - Build and start the Synthetics Agent
   - Agent fetches probe from API
   - Validate agent successfully retrieved the probe
   - Check probe status updates

4. **API Validation** (`API_Has_Probe_With_Valid_Status`)
   - Query API for the probe
   - Validate probe has correct properties (ID, URL, labels, status)
   - Verify cluster-id label matches

5. **Cleanup** (`RMO_Deletes_Probe`)
   - Delete probe via API
   - Verify probe is marked as terminating/deleted

## Running the Test

### Prerequisites

The test will automatically pull dependencies from Go modules. However, for local development:

**Option 1: Use environment variables (recommended)**
```bash
export RHOBS_SYNTHETICS_API_PATH=/path/to/rhobs-synthetics-api
export RHOBS_SYNTHETICS_AGENT_PATH=/path/to/rhobs-synthetics-agent
```

**Option 2: Use go.mod replace directives**
```go
// Add to go.mod:
replace github.com/rhobs/rhobs-synthetics-agent => /path/to/rhobs-synthetics-agent
replace github.com/rhobs/rhobs-synthetics-api => /path/to/rhobs-synthetics-api
```

Then run:
```bash
go mod tidy
```

### Run the Test

```bash
# From route-monitor-operator root:
cd test/e2e
go test -v -run TestFullStackIntegration -timeout 5m
```

### Short Mode (Skip Integration Tests)

```bash
go test -v -short
```

## Key Design Decisions

### 1. **External Repository Management**

The test now lives in the RMO repository and treats both the API and Agent as external dependencies:
- **RMO code**: Local imports (we're in the RMO repo)
- **Agent code**: External import, managed as subprocess (because agent package is internal)
- **API**: External, managed as subprocess

### 2. **Subprocess Architecture**

Both the API and Agent are built and run as subprocesses rather than imported as libraries:
- **Benefit**: Isolates test from internal package restrictions
- **Benefit**: Tests the actual binaries users will run
- **Benefit**: Simulates real production deployment

### 3. **API Types**

Since the agent's API types are in an `internal` package, they're redefined locally in `full_integration_helpers.go`:
```go
type Probe struct {
    ID        string            `json:"id"`
    StaticURL string            `json:"static_url"`
    Labels    map[string]string `json:"labels"`
    Status    string            `json:"status,omitempty"`
}
```

### 4. **Fake Kubernetes Client**

RMO reconciliation uses `sigs.k8s.io/controller-runtime/pkg/client/fake` to:
- Test RMO logic without a real Kubernetes cluster
- Create HostedControlPlane CRs in memory
- Validate RMO's reconciliation behavior

## Expected Test Output

```
=== RUN   TestFullStackIntegration
    ✅ Mock Dynatrace server started
    ✅ Mock probe target server started
    ✅ API server started
    ✅ RMO API proxy started
=== RUN   TestFullStackIntegration/RMO_Creates_Probe_From_HCP_CR
    ✅ Created HostedControlPlane CR
    ✅ RMO log found: Reconciling HostedControlPlanes
    ✅ RMO log found: Deploying internal monitoring objects
    ✅ RMO log found: Deploying HTTP Monitor Resources
    ✅ RMO log found: Deploying RHOBS probe
    ✅ RMO successfully created probe via API!
    ✅ API path proxy is working correctly
=== RUN   TestFullStackIntegration/Agent_Fetches_And_Processes_Probe
    ✅ Agent fetched probe
    ✅ Agent shut down successfully
=== RUN   TestFullStackIntegration/API_Has_Probe_With_Valid_Status
    ✅ Probe has valid status
    ✅ Probe has correct cluster-id label
=== RUN   TestFullStackIntegration/RMO_Deletes_Probe
    ✅ Successfully deleted probe
PASS
```

## Troubleshooting

### Port Conflicts
The API starts on port 8081+ (8080 is reserved for agent metrics). If you see port conflicts:
```bash
# Kill any processes using the ports
lsof -ti:8081 | xargs kill -9
lsof -ti:8082 | xargs kill -9
```

### Build Failures
If the API or Agent fails to build:
```bash
# Manually test building
cd /path/to/rhobs-synthetics-api && make build
cd /path/to/rhobs-synthetics-agent && make build
```

### Module Cache Issues
The test automatically copies modules from the Go cache to a temp directory for building. If you see permission errors:
```bash
# Clean Go caches
go clean -cache -modcache -testcache
```

## Migration Notes

This test was migrated from `rhobs-synthetics-agent/test/e2e/` to `route-monitor-operator/test/e2e/` with the following changes:

1. **Imports Updated**:
   - RMO imports: No change (still using `github.com/openshift/route-monitor-operator/...`)
   - Agent imports: Changed from internal import to subprocess execution
   - API types: Redefined locally (were previously in agent's internal package)

2. **Dependencies Added to go.mod**:
   - `github.com/rhobs/rhobs-synthetics-agent` - Added for agent binary building

3. **New Files Created**:
   - `agent_manager.go` - Manages agent subprocess lifecycle

4. **Files Moved**:
   - `full_integration_test.go` - Updated imports and agent execution
   - `full_integration_helpers.go` - Added local API type definitions
   - `api_manager.go` - Updated relative paths for RMO repository structure

## Future Enhancements

- Add metrics validation (verify Prometheus metrics are exported)
- Add Kubernetes resource validation (verify Probe CRs are created)
- Add multi-probe scenarios
- Add failure/recovery testing
- Add performance benchmarking


