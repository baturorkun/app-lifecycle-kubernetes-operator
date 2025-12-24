# App Lifecycle Kubernetes Operator

A Kubernetes operator that manages the lifecycle of applications within namespaces by freezing and resuming Deployments and StatefulSets. Perfect for scheduled maintenance, cost optimization, and ensuring consistent application state across operator restarts.

## Features

### 🎯 Freeze & Resume Operations
- **Freeze**: Scale down all (or selected) Deployments and StatefulSets to 0 replicas
- **Resume**: Restore original replica counts from stored annotations
- Supports label selectors to target specific workloads

### 🔄 Smart Startup Policy
When the operator starts (restart, node reboot, upgrade):
- **Smart Reconciliation**: Only applies action if current state differs from desired state
- **Idempotent**: Safe to run multiple times, won't duplicate operations
- **Status Tracking**: Records timestamp and action in resource status

### 🎫 Operation Idempotency
- Prevents duplicate operations using `operationId`
- Change the `operationId` to trigger a new operation
- Status tracks last processed operation ID

### 🏷️ Label Selector Support
Filter which Deployments and StatefulSets to affect:
```yaml
selector:
  matchLabels:
    app: myapp
  matchExpressions:
  - key: tier
    operator: In
    values: [frontend, backend]
```

### ⏱️ Custom Termination Grace Periods
Override graceful shutdown times during Freeze operations:
- Set custom `terminationGracePeriodSeconds` for faster shutdowns
- Separate values for Deployments and StatefulSets
- Original values automatically restored on Resume
- Range: 0-300 seconds

Example:
```yaml
spec:
  action: Freeze
  terminationGracePeriodSeconds:
    deployment: 15   # Fast shutdown for stateless apps
    statefulSet: 60  # Longer for stateful apps
```

### 🚀 Startup Node Readiness Policy
Wait for worker nodes to be ready before applying startup policy:
- **Prevents uneven pod distribution**: Ensures pods spread across all nodes
- **Two modes**: Wait for ALL nodes or minimum N nodes
- **Configurable timeout**: Max wait time before proceeding
- **Node selector**: Target specific node types (e.g., only worker nodes)
- **Status tracking**: Records how many nodes were ready and wait time

Example:
```yaml
spec:
  startupPolicy: Resume
  startupNodeReadinessPolicy:
    enabled: true
    requireAllNodes: true    # Wait for ALL worker nodes (required field)
    minReadyNodes: 2         # Only used when requireAllNodes: false
    timeoutSeconds: 120      # Wait max 2 minutes
    nodeSelector:
      node-role.kubernetes.io/worker: ""
```

### 📊 Comprehensive Status Tracking
- **Phase**: Current lifecycle state (Idle, Freezing, Frozen, Resuming, Resumed, Failed)
- **Message**: Human-readable status messages
- **LastHandledOperationId**: Tracks processed operations
- **LastStartupAt**: When operator last checked startup policy
- **LastStartupAction**: What action was taken at startup
- **LastResumeAt**: Timestamp of last resume operation (used for pod balancing)
- **StartupReadyNodes**: How many nodes were ready during startup
- **StartupNodesWaited**: How many seconds waited for nodes during startup

### ⚖️ Automatic Pod Balancing
When resuming workloads, pods may end up unevenly distributed if nodes become Ready at different times:
- **Problem**: Resume happens with 2 nodes ready → all pods go to those 2 nodes → 3rd node comes up later → stays empty
- **Solution**: Enable `balancePods: true` to automatically rebalance pods when new nodes become Ready
- **Time Window**: Set `balanceWindowSeconds` to control how long after resume the operator watches for new nodes
- **Event-Driven**: Operator watches node Ready events and triggers rolling restart when needed
- **Automatic**: No manual intervention required - pods redistribute across all available nodes

Example:
```yaml
spec:
  action: Resume
  balancePods: true
  balanceWindowSeconds: 600  # Watch for new nodes for 10 minutes
```

## Quick Start

### Installation

**Option 1: Run locally (recommended for development)**
```sh
make install  # Install CRDs
make run      # Run operator locally
```

**Option 2: Deploy to cluster**
```sh
make install
make deploy IMG=<your-registry>/app-lifecycle-kubernetes-operator:tag
```

**Create a sample policy:**
```sh
kubectl apply -f config/samples/apps_v1alpha1_namespacelifecyclepolicy.yaml
```

For detailed installation instructions, deployment options, and troubleshooting, see [INSTALL.md](INSTALL.md).

## Usage Examples

### Example 1: Freeze a Namespace (All Resources)

```yaml
apiVersion: apps.ops.dev/v1alpha1
kind: NamespaceLifecyclePolicy
metadata:
  name: freeze-production
  namespace: default
spec:
  targetNamespace: production
  action: Freeze
  operationId: "op-20231215-001"
  startupPolicy: Ignore
```

This will:
- Freeze ALL Deployments and StatefulSets in the `production` namespace
- Store original replica counts in annotations
- Set `startupPolicy: Ignore` means no action on operator restart

### Example 2: Resume with Startup Policy

```yaml
apiVersion: apps.ops.dev/v1alpha1
kind: NamespaceLifecyclePolicy
metadata:
  name: resume-dev
  namespace: default
spec:
  targetNamespace: dev
  action: Resume
  operationId: "op-20231215-002"
  startupPolicy: Resume
```

This will:
- Resume all frozen Deployments/StatefulSets in `dev` namespace
- On operator restart, automatically resume if namespace is frozen
- Smart: Only resumes if current state differs from desired state

### Example 3: Freeze Specific Apps Using Selector

```yaml
apiVersion: apps.ops.dev/v1alpha1
kind: NamespaceLifecyclePolicy
metadata:
  name: freeze-selected
  namespace: default
spec:
  targetNamespace: staging
  action: Freeze
  operationId: "op-20231215-003"
  startupPolicy: Freeze
  selector:
    matchLabels:
      app: web-app
      tier: frontend
```

This will:
- Only freeze Deployments/StatefulSets with matching labels
- On operator restart, ensures they remain frozen

### Example 4: Scheduled Freeze for Cost Savings

```yaml
apiVersion: apps.ops.dev/v1alpha1
kind: NamespaceLifecyclePolicy
metadata:
  name: nightly-freeze
  namespace: default
spec:
  targetNamespace: test-environment
  action: Freeze
  operationId: "nightly-2023121500"  # Change daily via CronJob
  startupPolicy: Ignore
```

Use with a CronJob to freeze test environments overnight:
- Change `operationId` daily to trigger new freeze operation
- `startupPolicy: Ignore` prevents auto-resume on operator restart

### Example 5: Resume with Automatic Pod Balancing

```yaml
apiVersion: apps.ops.dev/v1alpha1
kind: NamespaceLifecyclePolicy
metadata:
  name: resume-with-balancing
  namespace: default
spec:
  targetNamespace: production
  action: Resume
  operationId: "resume-20231216-001"
  startupPolicy: Resume
  balancePods: true
  balanceWindowSeconds: 600  # 10 minutes
```

This will:
- Resume all frozen Deployments/StatefulSets in `production` namespace
- Watch for new nodes becoming Ready for the next 10 minutes
- Automatically trigger rolling restart when new nodes join
- Ensure pods are evenly distributed across all available nodes
- Perfect for scenarios where nodes start at different times (cluster scaling, node maintenance)

### Example 6: Fast Freeze with Custom Grace Periods

```yaml
apiVersion: apps.ops.dev/v1alpha1
kind: NamespaceLifecyclePolicy
metadata:
  name: fast-freeze
  namespace: default
spec:
  targetNamespace: test-environment
  action: Freeze
  operationId: "fast-freeze-001"
  startupPolicy: Freeze
  terminationGracePeriodSeconds:
    deployment: 10      # Quick shutdown for stateless apps
    statefulSet: 30     # More time for stateful apps
```

This will:
- Freeze all workloads with custom termination grace periods
- Deployments get 10 seconds to shutdown (instead of default 30)
- StatefulSets get 30 seconds (instead of default)
- Original grace periods stored in annotations
- Automatically restored on Resume

### Example 7: Startup with Node Readiness Check

```yaml
apiVersion: apps.ops.dev/v1alpha1
kind: NamespaceLifecyclePolicy
metadata:
  name: production-policy
  namespace: default
spec:
  targetNamespace: production
  action: Resume
  operationId: "prod-startup-001"
  startupPolicy: Resume
  startupNodeReadinessPolicy:
    enabled: true
    requireAllNodes: true    # Wait for ALL worker nodes
    timeoutSeconds: 120      # Max 2 minutes wait
    nodeSelector:
      node-role.kubernetes.io/worker: ""
```

This will:
- Wait for ALL worker nodes to be Ready before resuming (up to 2 minutes)
- Prevents uneven pod distribution when cluster starts
- Perfect for operator restarts, node reboots, cluster upgrades
- Status shows how many nodes were ready and wait time
- If timeout reached, proceeds with available nodes

**Alternative - Wait for minimum nodes:**
```yaml
startupNodeReadinessPolicy:
  enabled: true
  requireAllNodes: false   # Use minReadyNodes instead
  minReadyNodes: 2         # Wait for at least 2 nodes
  timeoutSeconds: 120
```

## API Reference

### NamespaceLifecyclePolicySpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `targetNamespace` | string | Yes | The namespace to apply this policy to |
| `action` | enum | Yes | `Freeze` or `Resume` |
| `operationId` | string | No | Unique ID for operation idempotency |
| `startupPolicy` | enum | Yes | `Ignore`, `Freeze`, or `Resume` - action on operator startup |
| `selector` | LabelSelector | No | Filter resources by labels (all if omitted) |
| `balancePods` | boolean | No | Enable automatic pod redistribution when nodes become Ready (default: false) |
| `balanceWindowSeconds` | int32 | No | Time window in seconds for pod balancing after resume (default: 600, max: 3600) |
| `terminationGracePeriodSeconds` | object | No | Override graceful shutdown time during Freeze (restored on Resume) |
| `terminationGracePeriodSeconds.deployment` | int64 | No | Grace period for Deployments (0-300 seconds) |
| `terminationGracePeriodSeconds.statefulSet` | int64 | No | Grace period for StatefulSets (0-300 seconds) |
| `startupNodeReadinessPolicy` | object | No | Wait for nodes to be ready before applying startup policy |
| `startupNodeReadinessPolicy.enabled` | boolean | No | Enable node readiness check (default: false) |
| `startupNodeReadinessPolicy.requireAllNodes` | boolean | **Yes*** | Wait for ALL nodes (true) or use minReadyNodes (false). *Required when startupNodeReadinessPolicy is set |
| `startupNodeReadinessPolicy.minReadyNodes` | int32 | No | Minimum ready nodes required (only used when requireAllNodes is false, default: 1) |
| `startupNodeReadinessPolicy.timeoutSeconds` | int32 | No | Max wait time for nodes (default: 60, max: 600) |
| `startupNodeReadinessPolicy.nodeSelector` | map | No | Select which nodes to count (default: `{"node-role.kubernetes.io/worker": ""}`) |

### NamespaceLifecyclePolicyStatus

| Field | Type | Description |
|-------|------|-------------|
| `phase` | enum | Current phase: `Idle`, `Freezing`, `Frozen`, `Resuming`, `Resumed`, `Failed` |
| `lastHandledOperationId` | string | Last processed operation ID |
| `message` | string | Human-readable status message |
| `lastStartupAt` | timestamp | When startup policy was last checked |
| `lastStartupAction` | string | Action taken at startup (e.g., `FREEZE_APPLIED`, `NO_ACTION_ALREADY_FROZEN`) |
| `lastResumeAt` | timestamp | When last Resume operation completed (used for pod balancing time window) |
| `startupReadyNodes` | int32 | How many nodes were ready during startup (when node readiness check enabled) |
| `startupNodesWaited` | int32 | How many seconds waited for nodes during startup |
| `conditions` | []Condition | Kubernetes standard conditions (reserved for future use) |

### Startup Policy Actions

When the operator starts, it checks each policy's `startupPolicy`:

| startupPolicy | Current Phase | Action |
|---------------|---------------|--------|
| `Ignore` | Any | No action taken |
| `Freeze` | `Frozen` | No action (already frozen) ✅ |
| `Freeze` | `Resumed` | Freeze namespace 🥶 |
| `Resume` | `Resumed` | No action (already resumed) ✅ |
| `Resume` | `Frozen` | Resume namespace ▶️ |

Status records the result in `lastStartupAction`:
- `FREEZE_APPLIED` - Froze the namespace
- `RESUME_APPLIED` - Resumed the namespace
- `NO_ACTION_ALREADY_FROZEN` - Already in frozen state
- `NO_ACTION_ALREADY_RESUMED` - Already in resumed state
- `SKIPPED_IGNORE` - StartupPolicy set to Ignore
- `SKIPPED_NAMESPACE_NOT_FOUND` - Target namespace doesn't exist

## How It Works

### Freeze Operation
1. Lists all Deployments and StatefulSets in target namespace (filtered by selector if provided)
2. For each resource:
   - Stores current replica count in annotation `apps.ops.dev/original-replicas`
   - Sets replicas to 0
3. Updates status to `Frozen`
4. Records `operationId` to prevent duplicate operations

### Resume Operation
1. Lists all Deployments and StatefulSets in target namespace
2. For each resource:
   - Reads original replica count from annotation
   - Restores original replica count
   - Removes the annotation
3. Updates status to `Resumed`

### Startup Reconciliation
When operator starts:
1. Reads all NamespaceLifecyclePolicy resources
2. For each policy with `startupNodeReadinessPolicy` enabled:
   - Waits for nodes to be ready (based on `requireAllNodes` setting)
   - If `requireAllNodes: true` → counts total nodes and waits for all
   - If `requireAllNodes: false` → waits for `minReadyNodes`
   - Records `startupReadyNodes` and `startupNodesWaited` in status
   - Proceeds after timeout if nodes not ready
3. For each policy:
   - Checks `startupPolicy` field
   - Compares current `phase` with desired state
   - Only applies action if states differ (smart reconciliation)
   - Updates `lastStartupAt` timestamp
   - Records action in `lastStartupAction`

### Termination Grace Period Override
During Freeze with `terminationGracePeriodSeconds` set:
1. For each Deployment/StatefulSet:
   - Stores original `terminationGracePeriodSeconds` in annotation `apps.ops.dev/original-termination-grace-period`
   - Applies custom grace period from policy spec
2. During Resume:
   - Restores original grace period from annotation
   - Removes the annotation

## Monitoring

Check policy status:
```sh
kubectl get namespacelifecyclepolicy -A
kubectl describe namespacelifecyclepolicy freeze-production
```

View detailed status:
```sh
kubectl get namespacelifecyclepolicy freeze-production -o yaml
```

Example output:
```yaml
status:
  phase: Frozen
  message: Successfully froze 2 deployments and 1 statefulsets
  lastHandledOperationId: "op-20231215-001"
  lastStartupAt: "2025-12-24T06:00:43Z"
  lastStartupAction: "RESUME_APPLIED"
  startupReadyNodes: 3
  startupNodesWaited: 5
```

## Development

**Quick development workflow:**
```sh
make install   # Install CRDs
make run       # Run operator locally
make test      # Run tests
```

**After modifying API types:**
```sh
make generate  # Update generated code
make manifests # Update CRD manifests
make install   # Re-install CRDs
```

For detailed development workflow, building images, and deployment options, see [INSTALL.md](INSTALL.md).

## Use Cases

### 1. Scheduled Environment Shutdown
Save costs by freezing dev/test environments during off-hours.

**Freeze policy (triggered at 6 PM):**
```yaml
spec:
  targetNamespace: dev-environment
  action: Freeze
  operationId: "freeze-20231215-1800"
  startupPolicy: Ignore
```

**Resume policy (triggered at 8 AM):**
```yaml
spec:
  targetNamespace: dev-environment
  action: Resume
  operationId: "resume-20231216-0800"
  startupPolicy: Resume  # Auto-resume if operator restarts
```

Use a CronJob to update the operationId and trigger these policies at scheduled times.

### 2. Node Maintenance
Gracefully freeze workloads before node maintenance:
```yaml
spec:
  targetNamespace: production
  action: Freeze
  operationId: "maintenance-20231215"
  startupPolicy: Freeze  # Keep frozen if operator restarts during maintenance
```

### 3. Disaster Recovery Testing
Freeze production-like environments for backup/snapshot testing:
```yaml
spec:
  targetNamespace: prod-replica
  action: Freeze
  operationId: "dr-test-001"
  selector:
    matchLabels:
      backup-enabled: "true"
  startupPolicy: Freeze
```

### 4. Canary Deployments
Freeze old version while testing new version:
```yaml
spec:
  targetNamespace: production
  action: Freeze
  selector:
    matchLabels:
      version: "v1"
  startupPolicy: Ignore
```

## Troubleshooting

**Policy not taking effect:**
- Check if `operationId` is the same as last run (change it to trigger new operation)
- Verify target namespace exists
- Check operator logs: `kubectl logs -n <operator-namespace> <pod-name>`

**Resources not frozen/resumed:**
- Check selector matches your resources: `kubectl get deploy,sts -n <namespace> --show-labels`
- Verify RBAC permissions are correct
- Check status message for errors

**Startup policy not working:**
- Verify `startupPolicy` is set (required field)
- Check `lastStartupAt` and `lastStartupAction` in status
- Operator only checks on startup, not during runtime

## Contributing

We welcome contributions! To contribute:

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests and run `make test`
5. Update documentation if needed
6. Run `make manifests` to update CRDs
7. Submit a pull request

For detailed guidelines, see [INSTALL.md](INSTALL.md#development-workflow).

## License

Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
