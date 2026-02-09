#!/bin/bash
set -euo pipefail

LOG=/var/log/rke2-pre-shutdown.log
# Ensure the log exists and is writable; during shutdown some filesystems may be readonly so also send to systemd journal
touch "$LOG" 2>/dev/null || true
chmod 0644 "$LOG" 2>/dev/null || true
log() {
  local msg="$1"
  # Try file append (may fail if FS is readonly)
  echo "$msg" >> "$LOG" 2>/dev/null || true
  # Also write to stderr so systemd captures it in the journal
  echo "$msg" >&2
  # And use logger to ensure it reaches the journal/syslog
  logger -t rke2-pre-shutdown "$msg" || true
}
log "$(date -Is) 🔔 Pre-shutdown started"

export PATH=$PATH:/var/lib/rancher/rke2/bin
export KUBECONFIG=/etc/rancher/rke2/rke2.yaml

NAMESPACE=default
CR_KIND=namespacelifecyclepolicies

TIMESTAMP=$(date -Is)
NODE_NAME=$(hostname)

log "$(date -Is) 📦 Fetching NamespaceLifecyclePolicy CRs"

# 1. Get the list of CRs (strip empty lines)
CR_LIST=$(kubectl -n "$NAMESPACE" get "$CR_KIND" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sed '/^\s*$/d')

# 2. Identify unique Namespaces to watch (strip empty entries)
# (Assuming the CR name or a field within it maps to the namespace being managed)
# If the CRs are in 'default' but manage other namespaces, 
# we extract the target namespace from the CR spec.
TARGET_NAMESPACES=$(kubectl -n "$NAMESPACE" get "$CR_KIND" -o jsonpath='{range .items[*]}{.spec.targetNamespace}{"\n"}{end}' | sed '/^\s*$/d' | sort -u)

# Convert into arrays for safer iteration
readarray -t CR_ARRAY <<< "$CR_LIST"
readarray -t TARGET_ARRAY <<< "$TARGET_NAMESPACES"

# 3. Patch the CRs
for CR in "${CR_ARRAY[@]}"; do
  [ -z "$CR" ] && continue
  log "$(date -Is) ✏️ Patching $CR"
  kubectl -n "$NAMESPACE" patch "$CR_KIND" "$CR" \
    --type merge \
    -p "{\"spec\":{\"operationId\":\"$NODE_NAME-$TIMESTAMP\"}}"
done

# 4. Wait for Pods in those specific namespaces to disappear
MAX_WAIT_SECONDS=300
ELAPSED=0

# If no target namespaces were found, skip waiting
if [ "${#TARGET_ARRAY[@]}" -eq 0 ]; then
  log "$(date -Is) ⚠️ No target namespaces found; skipping pod wait."
else
  log "$(date -Is) ⏳ Waiting for pods in namespaces: [ ${TARGET_ARRAY[*]} ]"

  while [ $ELAPSED -lt $MAX_WAIT_SECONDS ]; do
    TOTAL_PODS=0

    for NS in "${TARGET_ARRAY[@]}"; do
      [ -z "$NS" ] && continue
      # Count pods that are not in a terminal state (Succeeded/Failed)
      COUNT=$(kubectl get pods -n "$NS" --no-headers 2>/dev/null | grep -vE "Succeeded|Failed" | wc -l || echo 0)
      # Ensure COUNT is a clean integer
      COUNT=$(echo "$COUNT" | tr -d '[:space:]')
      TOTAL_PODS=$((TOTAL_PODS + COUNT))
    done

    if [ "$TOTAL_PODS" -eq 0 ]; then
      log "$(date -Is) 🏁 All relevant namespaces are clear."
      break
    fi

    log "$(date -Is) ⏳ Still $TOTAL_PODS pods remaining in target namespaces... ($ELAPSED/$MAX_WAIT_SECONDS s)"
    sleep 5
    ELAPSED=$((ELAPSED + 5))
  done
fi

log "$(date -Is) 🚀 Script finished."