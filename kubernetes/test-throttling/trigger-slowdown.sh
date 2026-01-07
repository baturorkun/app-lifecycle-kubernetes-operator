#!/usr/bin/env bash

# Script: kubernetes/test-throttling/trigger-slowdown.sh
# Purpose: Create resource-constrained pending pods to trigger adaptive throttling slowdown (🐌)

set -e

NS="throttle-trigger-ns"
THRESHOLD=10

echo "===================================================="
echo "🚀 Triggering Adaptive Throttling Slowdown"
echo "===================================================="
echo "1. Creating namespace: $NS"
kubectl create namespace "$NS" 2>/dev/null || echo "Namespace $NS already exists"

echo "2. Creating resource hog deployment (High CPU requests)"
echo "   This will create pods that stay in 'Pending' (Unschedulable)."

cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: resource-hog
  namespace: $NS
spec:
  replicas: $((THRESHOLD + 5))
  selector:
    matchLabels:
      app: resource-hog
  template:
    metadata:
      labels:
        app: resource-hog
    spec:
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.9
        resources:
          requests:
            cpu: "100" # This will definitely fail to schedule
EOF

echo ""
echo "===================================================="
echo "✅ Setup complete!"
echo "===================================================="
echo "Pod status in $NS:"
kubectl get pods -n "$NS"
echo ""
echo "Next Steps:"
echo "1. Check if pods are 'Unschedulable' due to 'Insufficient cpu':"
echo "   kubectl describe pod -n $NS | grep Insufficient"
echo ""
echo "2. Run your 'Resume' operation in another namespace."
echo "   The operator (monitoring cluster-wide) will see these pending pods"
echo "   and log: 🐌 Throttling: Adjusted batch size"
echo ""
echo "3. After testing, clean up:"
echo "   kubectl delete namespace $NS"
echo "===================================================="
