#!/bin/bash
set -euo pipefail

LOG=/var/log/rke2-pre-shutdown.log
touch "$LOG" 2>/dev/null || true
chmod 0644 "$LOG" 2>/dev/null || true

log() {
  local msg="$1"
  echo "$msg" >> "$LOG" 2>/dev/null || true
  echo "$msg" >&2
  logger -t rke2-pre-shutdown "$msg" || true
}

log "$(date -Is) Pre-shutdown started"

export PATH=$PATH:/var/lib/rancher/rke2/bin
export KUBECONFIG=/etc/rancher/rke2/rke2.yaml

CR_KIND=namespacelifecyclepolicies
TIMESTAMP=$(date -Is)
NODE_NAME=$(hostname)
OPERATION_ID="${NODE_NAME}-shutdown-${TIMESTAMP}"

MAX_WAIT_SECONDS=300
POLL_INTERVAL=5

# Get all CRs across all namespaces: lines of "namespace/name"
CR_LIST=$(kubectl get "$CR_KIND" --all-namespaces \
  -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{ .metadata.name}{"\n"}{end}' \
  | sed '/^\s*$/d') || true

if [ -z "$CR_LIST" ]; then
  log "$(date -Is) No NamespaceLifecyclePolicy CRs found, nothing to do."
  exit 0
fi

readarray -t CR_ARRAY <<< "$CR_LIST"
log "$(date -Is) Found ${#CR_ARRAY[@]} CR(s): ${CR_ARRAY[*]}"

# Patch all CRs: set action=Freeze and a new operationId
for ENTRY in "${CR_ARRAY[@]}"; do
  [ -z "$ENTRY" ] && continue
  NS="${ENTRY%%/*}"
  CR="${ENTRY##*/}"
  log "$(date -Is) Patching $NS/$CR -> action=Freeze operationId=$OPERATION_ID"
  kubectl -n "$NS" patch "$CR_KIND" "$CR" \
    --type merge \
    -p "{\"spec\":{\"action\":\"Freeze\",\"operationId\":\"${OPERATION_ID}\"}}" \
    || log "$(date -Is) WARNING: failed to patch $NS/$CR (continuing)"
done

# Wait for all CRs to reach Frozen phase AND all pods to terminate
log "$(date -Is) Waiting up to ${MAX_WAIT_SECONDS}s for all CRs to reach Frozen phase and pods to terminate..."

ELAPSED=0
while [ $ELAPSED -lt $MAX_WAIT_SECONDS ]; do
  ALL_DONE=true
  SUMMARY=""

  for ENTRY in "${CR_ARRAY[@]}"; do
    [ -z "$ENTRY" ] && continue
    NS="${ENTRY%%/*}"
    CR="${ENTRY##*/}"

    # Get CR details
    PHASE=$(kubectl -n "$NS" get "$CR_KIND" "$CR" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
    TARGET_NS=$(kubectl -n "$NS" get "$CR_KIND" "$CR" -o jsonpath='{.spec.targetNamespace}' 2>/dev/null || echo "$NS")

    if [ "$PHASE" != "Frozen" ]; then
      ALL_DONE=false
      SUMMARY="${SUMMARY} ${NS}/${CR}=${PHASE}"
    else
      # Phase is Frozen, now check for active pods in target namespace
      # We exclude pods in terminal states (Succeeded, Failed, Completed)
      POD_COUNT=$(kubectl -n "$TARGET_NS" get pods --no-headers 2>/dev/null | grep -vE "Succeeded|Failed|Completed" | sed '/^\s*$/d' | wc -l)
      if [ "$POD_COUNT" -gt 0 ]; then
        ALL_DONE=false
        SUMMARY="${SUMMARY} ${TARGET_NS}:pods=${POD_COUNT}"
      else
        SUMMARY="${SUMMARY} ${TARGET_NS}:ready"
      fi
    fi
  done

  if $ALL_DONE; then
    log "$(date -Is) All CRs are Frozen and pods terminated. Done."
    break
  fi

  log "$(date -Is) Waiting (${ELAPSED}/${MAX_WAIT_SECONDS}s):${SUMMARY}"
  sleep $POLL_INTERVAL
  ELAPSED=$((ELAPSED + POLL_INTERVAL))
done

if ! $ALL_DONE; then
  log "$(date -Is) WARNING: Timed out waiting for all CRs to reach Frozen or pods to terminate. Proceeding with shutdown anyway."
fi

log "$(date -Is) Pre-shutdown script finished."
