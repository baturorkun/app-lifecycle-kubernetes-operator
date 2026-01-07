#!/usr/bin/env bash

# Script: kubernetes/test-throttling/trigger-crashloop.sh
# Purpose: Create crashing pods to trigger SignalContainerRestarts (🐌)

set -e

NS="crash-trigger-ns"

echo "===================================================="
echo "🚀 Triggering Throttling via Container Restarts"
echo "===================================================="
echo "1. Creating namespace: $NS"
kubectl create namespace "$NS" 2>/dev/null || echo "Namespace $NS already exists"

echo "2. Creating crashing deployment (exit 1)"
echo "   This will cause CrashLoopBackOff."

cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: crasher
  namespace: $NS
spec:
  replicas: 5
  selector:
    matchLabels:
      app: crasher
  template:
    metadata:
      labels:
        app: crasher
    spec:
      containers:
      - name: oops
        image: busybox
        command: ["/bin/sh", "-c", "exit 1"] # Always fails
EOF

echo ""
echo "===================================================="
echo "✅ Setup complete!"
echo "===================================================="
echo "Pod status in $NS:"
kubectl get pods -n "$NS"
echo ""
echo "Next Steps:"
echo "1. Wait for pods to hit 'CrashLoopBackOff' or multiple restarts."
echo ""
echo "2. Ensure your policy has this config enabled:"
echo "   adaptiveThrottling:"
echo "     signalChecks:"
echo "       checkContainerRestarts:"
echo "         enabled: true"
echo "         restartThreshold: 10"
echo ""
echo "3. Run 'Resume' and check logs for:"
echo "   🐌 Throttling: Adjusted batch size due to container restarts"
echo "===================================================="
