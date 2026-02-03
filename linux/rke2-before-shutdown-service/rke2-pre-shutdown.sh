#!/bin/bash
set -euo pipefail

LOG=/var/log/rke2-pre-shutdown.log
echo "$(date -Is) 🔔 Pre-shutdown started" >> "$LOG"

export PATH=$PATH:/var/lib/rancher/rke2/bin
export KUBECONFIG=/etc/rancher/rke2/rke2.yaml

NAMESPACE=default
CR_KIND=namespacelifecyclepolicies

TIMESTAMP=$(date -Is)
NODE_NAME=$(hostname)

echo "$(date -Is) 📦 Fetching NamespaceLifecyclePolicy CRs" >> "$LOG"

# 1. Get the list of CRs
CR_LIST=$(kubectl -n "$NAMESPACE" get "$CR_KIND" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')

# 2. Identify unique Namespaces to watch 
# (Assuming the CR name or a field within it maps to the namespace being managed)
# If the CRs are in 'default' but manage other namespaces, 
# we extract the target namespace from the CR spec.
TARGET_NAMESPACES=$(kubectl -n "$NAMESPACE" get "$CR_KIND" -o jsonpath='{range .items[*]}{.spec.targetNamespace}{"\n"}{end}' | sort -u)

# 3. Patch the CRs
for CR in $CR_LIST; do
  echo "$(date -Is) ✏️ Patching $CR" >> "$LOG"
  kubectl -n "$NAMESPACE" patch "$CR_KIND" "$CR" \
    --type merge \
    -p "{\"spec\":{\"operationId\":\"$NODE_NAME-$TIMESTAMP\"}}"
done

# 4. Wait for Pods in those specific namespaces to disappear
MAX_WAIT_SECONDS=300
ELAPSED=0

echo "$(date -Is) ⏳ Waiting for pods in namespaces: [ $TARGET_NAMESPACES ]" >> "$LOG"

while [ $ELAPSED -lt $MAX_WAIT_SECONDS ]; do
  TOTAL_PODS=0
  
  for NS in $TARGET_NAMESPACES; do
    # Count pods that are not in a terminal state (Succeeded/Failed)
    COUNT=$(kubectl get pods -n "$NS" --no-headers 2>/dev/null | grep -vE "Succeeded|Failed" | wc -l || echo 0)
    TOTAL_PODS=$((TOTAL_PODS + COUNT))
  done

  if [ "$TOTAL_PODS" -eq 0 ]; then
    echo "$(date -Is) 🏁 All relevant namespaces are clear." >> "$LOG"
    break
  fi

  echo "$(date -Is) ⏳ Still $TOTAL_PODS pods remaining in target namespaces... ($ELAPSED/$MAX_WAIT_SECONDS s)" >> "$LOG"
  sleep 5
  ELAPSED=$((ELAPSED + 5))
done

echo "$(date -Is) 🚀 Script finished." >> "$LOG"