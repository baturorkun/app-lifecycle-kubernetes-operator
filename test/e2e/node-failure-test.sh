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

# ---- Cleanup trap (always restore cluster on exit) --------------------------
cleanup() {
    stop_operator 2>/dev/null || true
    podman start "${WORKER_NODE}" > /dev/null 2>&1 || true
}
trap cleanup EXIT

# ---- Config ----------------------------------------------------------------
POLICY_NS="default"
POLICY_TEST1="policy-test1"
POLICY_TEST2="policy-test2"
WORKER_NODE="dev-worker"
OPERATOR_BIN="./bin/manager"
OPERATOR_LOG="/tmp/op-node-failure-test.log"
STARTUP_TIMEOUT=90   # seconds
RESUME_TIMEOUT=120   # seconds
NODE_TIMEOUT=120     # seconds

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

wait_node_state() {
    local node=$1 state=$2 timeout=${3:-${NODE_TIMEOUT}}
    local elapsed=0
    until kubectl get node "${node}" --no-headers 2>/dev/null | grep -q "${state}"; do
        sleep 5; elapsed=$((elapsed+5))
        [[ ${elapsed} -ge ${timeout} ]] && fail "${node} did not become ${state} within ${timeout}s"
    done
    info "${node} is ${state} (after ${elapsed}s)"
}

patch_action() {
    local policy=$1 action=$2 suffix=$3
    local ts; ts=$(date +%Y%m%d-%H%M%S)
    local opid="${action,,}-${ts}-${suffix}"
    kubectl patch namespacelifecyclepolicy "${policy}" -n "${POLICY_NS}" --type=merge \
        -p "{\"spec\":{\"action\":\"${action}\",\"operationId\":\"${opid}\"}}" > /dev/null
    info "${policy}: action=${action} opId=${opid}"
}

wait_log() {
    local pattern=$1 timeout=${2:-${STARTUP_TIMEOUT}} label=${3:-"${pattern}"}
    local elapsed=0
    while true; do
        { grep -q "${pattern}" "${OPERATOR_LOG}" 2>/dev/null && break; } || true
        sleep 2; elapsed=$((elapsed+2))
        [[ ${elapsed} -ge ${timeout} ]] && fail "Expected log not found within ${timeout}s: ${label}"
    done
    pass "${label}"
}

# ============================================================
# Pre-run: ensure worker node is up (may be down from a previous failed run)
# ============================================================
info "Ensuring ${WORKER_NODE} is running before test starts..."
podman start "${WORKER_NODE}" > /dev/null 2>&1 || true
wait_node_state "${WORKER_NODE}" "Ready"

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
wait_log "Startup policy check completed" "${STARTUP_TIMEOUT}" "Startup completed"

# ============================================================
# STEP 3 — Freeze → pods = 0
# ============================================================
step "STEP 3: Freeze workloads"

patch_action "${POLICY_TEST1}" "Freeze" "test1"
patch_action "${POLICY_TEST2}" "Freeze" "test2"

wait_log "Successfully frozen all resources" 60 "Freeze completed"

# ============================================================
# STEP 4 — Restart operator → startup resume → pods Running
# ============================================================
step "STEP 4: Restart operator — startup resume from frozen state"

stop_operator
start_operator

wait_log "STARTUP RESUME OPERATION STARTING" 60 "Startup resume started"
wait_log "RESUME POLICIES COMPLETED" "${RESUME_TIMEOUT}" "Startup resume completed"

# ============================================================
# STEP 5 — Stop worker node
# ============================================================
step "STEP 5: Stop worker node (${WORKER_NODE})"

info "Stopping ${WORKER_NODE}..."
podman stop "${WORKER_NODE}" > /dev/null

wait_node_state "${WORKER_NODE}" "NotReady"
pass "${WORKER_NODE} is NotReady"

wait_log "NODE FAILURE RECOVERY RESUME STARTING" 30 "Live node-failure recovery triggered"
wait_log "Delayed startup resume completed successfully" "${RESUME_TIMEOUT}" "Live recovery resume completed"
wait_log "Force-deleting terminating pods after recovery resume" 10 "Force-delete terminating pods triggered"

# ============================================================
# STEP 6 — Restart operator with failed node → pre-scan + recovery
# ============================================================
step "STEP 6: Restart operator with ${WORKER_NODE} still NotReady"

stop_operator
start_operator

wait_log "NotReady nodes detected" 30 "Pre-scan detected NotReady node"
wait_log "NODE FAILURE RECOVERY RESUME STARTING" 60 "NODE FAILURE RECOVERY RESUME started"
wait_log "Delayed startup resume completed successfully" "${RESUME_TIMEOUT}" "Recovery resume completed"
wait_log "Force-deleting terminating pods after recovery resume" 10 "Force-delete terminating pods triggered"

info "Restoring ${WORKER_NODE}..."
podman start "${WORKER_NODE}" > /dev/null 2>&1 || true

# ============================================================
# Summary
# ============================================================
echo ""
echo -e "${GREEN}============================================================${NC}"
echo -e "${GREEN}  ALL 6 STEPS PASSED ✅${NC}"
echo -e "${GREEN}============================================================${NC}"
echo ""
echo "  Step 1: All nodes Ready                    ✅"
echo "  Step 2: Operator startup                   ✅"
 echo "  Step 3: Freeze completed                   ✅"
echo "  Step 4: Restart → startup resume           ✅"
echo "  Step 5: Node stopped → live recovery       ✅"
echo "  Step 6: Restart + pre-scan + recovery      ✅"
echo ""
echo "Operator log: ${OPERATOR_LOG}"
echo ""
