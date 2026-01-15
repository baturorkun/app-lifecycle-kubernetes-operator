#!/usr/bin/env bash

# Script: kubernetes/create-throttling-policies.sh
# Purpose: Create NamespaceLifecyclePolicy CRs for adaptive throttling testing

set -e

# === PARAMETER VALIDATION ===

# Support both direct arguments and NAMESPACES env variable
if [[ -z "$1" ]]; then
    if [[ -n "$NAMESPACES" ]]; then
        # Read from NAMESPACES environment variable
        read -ra NS_ARRAY <<< "$NAMESPACES"
        set -- "${NS_ARRAY[@]}"
    else
        echo "Error: Namespace list is required"
        echo ""
        echo "Usage Option 1 (direct arguments):"
        echo "  $0 <namespace1> <namespace2> <namespace3> ..."
        echo "  Example: $0 test-ns-1 test-ns-2 test-ns-3"
        echo ""
        echo "Usage Option 2 (environment variable):"
        echo "  export NAMESPACES=\"test-ns-1 test-ns-2 test-ns-3\""
        echo "  $0"
        exit 1
    fi
fi

NAMESPACES=("$@")
NAMESPACE_COUNT=${#NAMESPACES[@]}

echo "====================================="
echo "Creating Throttling Policies"
echo "====================================="
echo "Namespaces:      ${NAMESPACES[*]}"
echo "Count:           $NAMESPACE_COUNT"
echo "Action:          Freeze"
echo "StartupPolicy:   Resume"
echo "Priority:        1, 2, 3, ... (sequential, max 1000)"
echo "ResumeDelay:     ${DELAY}s (same for all policies)"
echo "====================================="

# === CREATE POLICIES ===

echo ""
echo "Creating policies..."

# Fixed delay for all policies (in seconds) - same for all policies
DELAY=0
PRIORITY=1
TIMESTAMP=$(date +%Y%m%d-%H%M%S)

for NS in "${NAMESPACES[@]}"; do
    POLICY_NAME="policy-$NS"
    OPERATION_ID="freeze-${TIMESTAMP}-${NS}"

    echo ""
    echo "[$PRIORITY/$NAMESPACE_COUNT] Creating policy: $POLICY_NAME"
    echo "  Target NS:     $NS"
    echo "  OperationID:   $OPERATION_ID"
    echo "  Priority:      $PRIORITY"
    echo "  ResumeDelay:   ${DELAY}s"

    cat <<EOF | kubectl apply -f -
apiVersion: apps.ops.dev/v1alpha1
kind: NamespaceLifecyclePolicy
metadata:
  name: $POLICY_NAME
  namespace: default
spec:
  # Target namespace to manage
  targetNamespace: $NS

  # Current action: Freeze all workloads
  action: Freeze

  # Startup policy: Resume when operator starts
  startupPolicy: Resume

  # Operation ID for idempotency and tracking
  operationId: "$OPERATION_ID"

  # Startup resume priority (lower number = higher priority)
  # Sequential priorities: 1000, 2000, 3000, ...
  startupResumePriority: $PRIORITY

  # Startup resume delay for staggering (prevents simultaneous resume burst)
  startupResumeDelay: ${DELAY}s

  # Adaptive throttling configuration for Resume operations
  adaptiveThrottling:
    enabled: true

    # Start with 3 workloads at a time
    initialBatchSize: 3

    # Never go below 1 workload per batch
    minBatchSize: 1

    # Wait 5 seconds between batches
    batchInterval: 3

    # Signal monitoring
    signalChecks:
      # Signal 1: Node Ready Status (Critical - STOP)
      checkNodeReady:
        enabled: true
        waitInterval: 20
        maxWaitTime: 1800

      # Signal 2: Node Pressure (Warning - SLOW DOWN)
      checkNodePressure:
        enabled: true
        pressureTypes:
          - MemoryPressure
          - DiskPressure
          - PIDPressure
        slowdownPercent: 50

      # Signal 3: Node Usage (Warning - PROACTIVE SLOW DOWN)
      # Monitors real-time CPU/memory usage from kubelet (~10s lag)
      # Or from custom metrics server via scrape configuration
      checkNodeUsage:
        enabled: true
        cpuThresholdPercent: 40
        memoryThresholdPercent: 80
        slowdownPercent: 60
        scrape:
          source: ":9090/metrics"
          cpu: "cpu_usages.percentage"
          mem: "memory_usages.percentage"

      # Signal 4: Pending Pods (Info - SLOW DOWN)
      # CLUSTER-WIDE: Counts resource-constrained pending pods across ALL namespaces
      checkPendingPods:
        enabled: true
        threshold: 5
        slowdownPercent: 70

      # Signal 5: Container Restarts (Warning - SLOW DOWN)
      # CLUSTER-WIDE: Detects pods in CrashLoopBackOff or with high restart counts
      checkContainerRestarts:
        enabled: true
        restartThreshold: 10
        slowdownPercent: 50

    # Monitor worker nodes
    nodeSelector:
      node-role.kubernetes.io/worker: ""

    # Fallback if metrics unavailable
    fallbackOnMetricsUnavailable: true

  # Select workloads with this label
  selector:
    matchLabels:
      app: test-throttle
EOF

    # Increment priority by 1 for next namespace (delay stays the same for all)
    PRIORITY=$((PRIORITY + 1))
done

# === SUMMARY ===

echo ""
echo "====================================="
echo "✅ Policies created successfully!"
echo "====================================="
echo ""
echo "Summary:"
kubectl get namespacelifecyclepolicy
echo ""
echo "Test workflow:"
echo "1. Create test workloads in each namespace:"
echo "   for ns in ${NAMESPACES[*]}; do"
echo "     IMAGE=nginx REPLICAS=3 NUMBER=10 TYPE=deployment ./kubernetes/test-create-deployments.sh \$ns"
echo "   done"
echo ""
echo "2. Policies will automatically Freeze workloads (action: Freeze)"
echo ""
echo "3. Wait for all freezes to complete:"
echo "   kubectl get namespacelifecyclepolicy --watch"
echo ""
echo "4. Simulate operator restart (delete and recreate policies, or restart operator)"
echo "   This will trigger startupPolicy: Resume with staggered delays"
echo ""
echo "5. Monitor adaptive throttling:"
echo "   kubectl get namespacelifecyclepolicy <policy-name> -o jsonpath='{.status.adaptiveProgress}' | jq"
echo ""
