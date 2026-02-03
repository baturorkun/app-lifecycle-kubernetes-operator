#!/bin/bash
set -euo pipefail

LOG=/var/log/rke2-pre-shutdown.log
echo "$(date -Is) 🔔 Pre-shutdown started" >> "$LOG"

export KUBECONFIG=/etc/rancher/rke2/rke2.yaml

NAMESPACE=default
CR_KIND=namespacelifecyclepolicies

TIMESTAMP=$(date -Is)
NODE_NAME=$(hostname)

echo "$(date -Is) 📦 Fetching NamespaceLifecyclePolicy CRs" >> "$LOG"

CR_LIST=$(kubectl -n "$NAMESPACE" get "$CR_KIND" \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')

for CR in $CR_LIST; do
  echo "$(date -Is) ✏️ Patching $CR" >> "$LOG"

  kubectl -n "$NAMESPACE" patch "$CR_KIND" "$CR" \
    --type merge \
    -p "{\"spec\":{\"operationId\":\"$NODE_NAME-$TIMESTAMP\"}}"
done

echo "$(date -Is) ✅ All CRs in default namespace patched" >> "$LOG"
