#!/usr/bin/env bash
# ============================================================
# node-failure-test.sh — End-to-end 6-step operator test
#
# Steps:
#   1. All nodes Ready
#   2. Run operator → startup resume → pods Running
#   3. Freeze → pods = 0
#   4. Restart operator → startup resume → pods Running
#   5. Stop a worker node (podman stop dev-worker)
#   6. Restart operator → pre-scan + NODE FAILURE RECOVERY RESUME
#
# Prerequisites:
#   - Kind cluster running (kubernetes/kind/start-3nodes_v2.sh)
#   - CRDs installed (make install)
#   - Test namespaces + policies created (kubernetes/create-resources.sh)
#   - Binary built (make build)
# ============================================================

set -euo pipefail

# ---- Config ----------------------------------------------------------------
POLICY_NS="default"
POLICY_TEST1="policy-test1"
POLICY_TEST2="policy-test2"
TARGET_NS1="test1"
TARGET_NS2="test2"
WORKER_NODE="dev-worker"
OPERATOR_BIN="./bin/manager"
OPERATOR_LOG="/tmp/op-node-failure-test.log"
DEPLOY_COUNT=20   # expected deployments per namespace
STARTUP_TIMEOUT=90  # seconds to wait for startup
FREEZE_TIMEOUT=30   # seconds to wait for freeze/resume to complete

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

pass() { echo -e "${GREEN}✅ $*${NC}"; }
fail() { echo -e "${RED}❌ $*${NC}"; exit 1; }
info() { echo -e "${BLUE}ℹ️  $*${NC}"; }
warn() { echo -e "${YELLOW}⚠️  $*${NC}"; }
step() { echo -e "\n${BLUE}==== $* ====${NC}"; }

# ---- Helpers ---------------------------------------------------------------

start_operator() {
    pkill -9 -f "bin/manager" 2>/dev/null || true
    sleep 1
    truncate -s 0 "${OPERATOR_LOG}"
    # Use nohup so Ctrl+C in the parent terminal doesn't send SIGINT to the operator
    nohup ${OPERATOR_BIN} >> "${OPERATOR_LOG}" 2>&1 &
    OPERATOR_PID=$!
    info "Operator started (PID=${OPERATOR_PID}), log → ${OPERATOR_LOG}"
}

stop_operator() {
    pkill -9 -f "bin/manager" 2>/dev/null || true
    sleep 1
}

wait_for_startup() {
    local timeout=${1:-${STARTUP_TIMEOUT}}
    local elapsed=0
    while ! grep -q "Startup policy check completed" "${OPERATOR_LOG}" 2>/dev/null; do
        sleep 2; elapsed=$((elapsed+2))
        [[ ${elapsed} -ge ${timeout} ]] && fail "Operator startup did not complete within ${timeout}s"
    done
    pass "Startup complete (${elapsed}s)"
}

running_pods() {
    kubectl get pods -n "$1" --no-headers 2>/dev/null | grep -c "Running" || echo 0
}

wait_pods() {
    local ns=$1 expected=$2 timeout=${3:-60}
    local elapsed=0
    while [[ "$(running_pods "${ns}")" -ne "${expected}" ]]; do
        sleep 3; elapsed=$((elapsed+3))
        [[ ${elapsed} -ge ${timeout} ]] && \
            fail "Pods in ${ns} did not reach ${expected} Running within ${timeout}s (current: $(running_pods "${ns}"))"
    done
    pass "Namespace ${ns}: ${expected} pods Running"
}

policy_phase() {
    kubectl get namespacelifecyclepolicy "$1" -n "${POLICY_NS}" \
        -o jsonpath='{.status.phase}' 2>/dev/null
}

wait_phase() {
    local policy=$1 expected=$2 timeout=${3:-${FREEZE_TIMEOUT}}
    local elapsed=0
    while [[ "$(policy_phase "${policy}")" != "${expected}" ]]; do
        sleep 2; elapsed=$((elapsed+2))
        [[ ${elapsed} -ge ${timeout} ]] && \
            fail "${policy} did not reach phase ${expected} within ${timeout}s (current: $(policy_phase "${policy}"))"
    done
    pass "${policy} phase = ${expected}"
}

patch_action() {
    local policy=$1 action=$2 suffix=$3
    local ts; ts=$(date +%Y%m%d-%H%M%S)
    local opid="${action,,}-${ts}-${suffix}"
    kubectl patch namespacelifecyclepolicy "${policy}" -n "${POLICY_NS}" --type=merge \
        -p "{\"spec\":{\"action\":\"${action}\",\"operationId\":\"${opid}\"}}" > /dev/null
    info "${policy}: action=${action} opId=${opid}"
}

log_contains() {
    grep -q "$1" "${OPERATOR_LOG}" 2>/dev/null
}

wait_log() {
    local pattern=$1 timeout=${2:-${STARTUP_TIMEOUT}}
    local elapsed=0
    while ! log_contains "${pattern}"; do
        sleep 2; elapsed=$((elapsed+2))
        [[ ${elapsed} -ge ${timeout} ]] && \
            fail "Log pattern '${pattern}' not found within ${timeout}s"
    done
}

# ============================================================
# STEP 1 — All nodes Ready
# ============================================================
step "STEP 1: Verify all nodes Ready"

NOT_READY=$(kubectl get nodes --no-headers 2>/dev/null | grep -v " Ready" | grep -v "^$" || true)
if [[ -n "${NOT_READY}" ]]; then
    fail "Some nodes are not Ready:\n${NOT_READY}"
fi
pass "All nodes Ready"
kubectl get nodes

# ============================================================
# STEP 2 — Run operator → startup resume → pods Running
# ============================================================
step "STEP 2: Start operator (startup resume)"

# Ensure policies are in Resume state with fresh opId so startup
# resume will process them even if previously frozen
info "Resetting policies to action=Resume"
patch_action "${POLICY_TEST1}" "Resume" "test1"
patch_action "${POLICY_TEST2}" "Resume" "test2"

# Clear any stale node failure state from previous test runs
kubectl patch namespacelifecyclepolicy "${POLICY_TEST1}" -n "${POLICY_NS}" \
    --type=merge --subresource=status \
    -p '{"status":{"failedNodeName":"","nodeFailureEventDetectedAt":null,"nodeFailureEventHandledAt":null,"pendingStartupResume":false,"pendingFreeze":false}}' \
    > /dev/null 2>&1 || true
kubectl patch namespacelifecyclepolicy "${POLICY_TEST2}" -n "${POLICY_NS}" \
    --type=merge --subresource=status \
    -p '{"status":{"failedNodeName":"","nodeFailureEventDetectedAt":null,"nodeFailureEventHandledAt":null,"pendingStartupResume":false,"pendingFreeze":false}}' \
    > /dev/null 2>&1 || true

start_operator
wait_for_startup

wait_pods "${TARGET_NS1}" "${DEPLOY_COUNT}" 120
wait_pods "${TARGET_NS2}" "${DEPLOY_COUNT}" 120

# Mark resume opId as handled (reconcile loop processes it)
sleep 5

# ============================================================
# STEP 3 — Freeze → pods = 0
# ============================================================
step "STEP 3: Freeze workloads"

patch_action "${POLICY_TEST1}" "Freeze" "test1"
patch_action "${POLICY_TEST2}" "Freeze" "test2"

# Spec update immediately triggers reconcile (generation changed)
sleep 3
wait_phase "${POLICY_TEST1}" "Frozen" 60
wait_phase "${POLICY_TEST2}" "Frozen" 60

# Verify replicas=0 and annotation written on a sample deployment
SAMPLE_REPLICAS=$(kubectl get deployment "${TARGET_NS1}-deploy-1" -n "${TARGET_NS1}" \
    -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "?")
SAMPLE_ANNOT=$(kubectl get deployment "${TARGET_NS1}-deploy-1" -n "${TARGET_NS1}" \
    -o jsonpath='{.metadata.annotations.apps\.ops\.dev/original-replicas}' 2>/dev/null || echo "")

[[ "${SAMPLE_REPLICAS}" == "0" ]] || fail "Expected replicas=0, got ${SAMPLE_REPLICAS}"
[[ -n "${SAMPLE_ANNOT}" ]] || fail "Missing apps.ops.dev/original-replicas annotation after freeze"
pass "Freeze verified: replicas=0, annotation=${SAMPLE_ANNOT}"

P1_PODS=$(kubectl get pods -n "${TARGET_NS1}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
P2_PODS=$(kubectl get pods -n "${TARGET_NS2}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
pass "Pods after freeze — test1: ${P1_PODS}, test2: ${P2_PODS}"

# ============================================================
# STEP 4 — Restart operator → startup resume → pods Running
# ============================================================
step "STEP 4: Restart operator — startup resume from frozen state"

stop_operator
start_operator
wait_for_startup

# Startup should detect phase=Frozen and run STARTUP RESUME
wait_log "STARTUP RESUME OPERATION STARTING" 60
pass "Startup resume fired"

wait_log "Adaptive throttling resume completed" 120
wait_pods "${TARGET_NS1}" "${DEPLOY_COUNT}" 120
wait_pods "${TARGET_NS2}" "${DEPLOY_COUNT}" 120

wait_phase "${POLICY_TEST1}" "Resumed" 30
wait_phase "${POLICY_TEST2}" "Resumed" 30
pass "All pods resumed after operator restart"

# ============================================================
# STEP 5 — Stop worker node
# ============================================================
step "STEP 5: Stop worker node (${WORKER_NODE})"

info "Pod distribution before stopping node:"
kubectl get pods -n "${TARGET_NS1}" -o wide --no-headers | awk '{print $7}' | sort | uniq -c || true

info "Stopping ${WORKER_NODE}..."
podman stop "${WORKER_NODE}" > /dev/null

# Wait for Kubernetes to mark it NotReady (node-monitor-grace-period = ~40s)
info "Waiting for ${WORKER_NODE} to become NotReady..."
ELAPSED=0
until kubectl get node "${WORKER_NODE}" --no-headers 2>/dev/null | grep -q "NotReady"; do
    sleep 5; ELAPSED=$((ELAPSED+5))
    [[ ${ELAPSED} -ge 120 ]] && fail "${WORKER_NODE} did not become NotReady within 120s"
done
pass "${WORKER_NODE} is NotReady (after ${ELAPSED}s)"

# The running operator detects node failure live via node-watcher
wait_log "NODE FAILURE RECOVERY RESUME STARTING" 30
pass "Live node-failure recovery triggered"

# ============================================================
# STEP 6 — Restart operator with failed node → pre-scan + recovery
# ============================================================
step "STEP 6: Restart operator with ${WORKER_NODE} still NotReady"

stop_operator
start_operator
wait_for_startup

# Startup pre-scan should detect the NotReady node
wait_log "NotReady nodes detected" 30
pass "Startup pre-scan detected NotReady node"

wait_log "Startup pre-scan: scaling down fully-local workloads" 30
pass "Pre-scan scale-down executed"

wait_log "NODE FAILURE RECOVERY RESUME STARTING" 60
pass "NODE FAILURE RECOVERY RESUME started"

wait_log "Adaptive throttling resume completed" 120
pass "Adaptive throttling resume completed"

wait_pods "${TARGET_NS1}" "${DEPLOY_COUNT}" 120
wait_pods "${TARGET_NS2}" "${DEPLOY_COUNT}" 120

pass "All ${DEPLOY_COUNT} pods Running in each namespace on surviving nodes"

# ============================================================
# Summary
# ============================================================
echo ""
echo -e "${GREEN}============================================================${NC}"
echo -e "${GREEN}  ALL 6 STEPS PASSED ✅${NC}"
echo -e "${GREEN}============================================================${NC}"
echo ""
echo "  Step 1: All nodes Ready                    ✅"
echo "  Step 2: Operator startup resume            ✅"
echo "  Step 3: Freeze → pods=0                    ✅"
echo "  Step 4: Restart → startup resume           ✅"
echo "  Step 5: Node stopped + live recovery       ✅"
echo "  Step 6: Restart + pre-scan + recovery      ✅"
echo ""
echo "Operator log: ${OPERATOR_LOG}"
echo ""

# Restore the cluster for future runs
info "Restoring ${WORKER_NODE}..."
podman start "${WORKER_NODE}" > /dev/null || true
