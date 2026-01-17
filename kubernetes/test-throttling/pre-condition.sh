#!/usr/bin/env bash

# Script: kubernetes/test-throttling/pre-condition.sh
# Purpose: Create a namespace and deployment for testing pre-conditions feature
#          The deployment waits 60 seconds before becoming ready

set -e

NS="pre-condition"
DEPLOYMENT_NAME="slow-app"

echo "====================================="
echo "Creating Pre-Condition Test Resources"
echo "====================================="
echo "Namespace:       $NS"
echo "Deployment:      $DEPLOYMENT_NAME"
echo "Ready Delay:     60 seconds"
echo "====================================="

# === CREATE NAMESPACE ===

echo ""
echo "Creating namespace: $NS"
kubectl create namespace "$NS" 2>/dev/null || echo "Namespace $NS already exists"

# === CREATE DEPLOYMENT ===

echo ""
echo "Creating deployment: $DEPLOYMENT_NAME"
echo "This deployment will wait 60 seconds before becoming ready..."

cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $DEPLOYMENT_NAME
  namespace: $NS
  labels:
    app: pre-condition-test
spec:
  replicas: 1
  selector:
    matchLabels:
      app: pre-condition-test
  template:
    metadata:
      labels:
        app: pre-condition-test
    spec:
      containers:
      - name: slow-app
        image: nginx:alpine
        ports:
        - containerPort: 80
          name: http
        # Readiness probe: waits 60 seconds before checking, so deployment won't be ready until then
        # This simulates an app that takes time to become ready
        readinessProbe:
          httpGet:
            path: /
            port: 80
          initialDelaySeconds: 60
          periodSeconds: 2
          timeoutSeconds: 1
          failureThreshold: 3
        # Liveness probe: keeps container alive (starts checking after readiness)
        livenessProbe:
          httpGet:
            path: /
            port: 80
          initialDelaySeconds: 90
          periodSeconds: 10
          timeoutSeconds: 1
          failureThreshold: 3
EOF

# === WAIT AND SHOW STATUS ===

echo ""
echo "Waiting for deployment to be created..."
sleep 2

echo ""
echo "====================================="
echo "Deployment Status"
echo "====================================="
kubectl get deployment "$DEPLOYMENT_NAME" -n "$NS" || true

echo ""
echo "Pod Status:"
kubectl get pods -n "$NS" -l app=pre-condition-test || true

echo ""
echo "====================================="
echo "✅ Pre-condition test resources created!"
echo "====================================="
echo ""
echo "The deployment '$DEPLOYMENT_NAME' will:"
echo "  - Pod starts immediately (container runs)"
echo "  - Readiness probe waits 60 seconds before checking"
echo "  - Deployment becomes ready after 60 seconds"
echo "  - This simulates an app that takes time to initialize"
echo ""
echo "To check readiness status:"
echo "  kubectl get deployment $DEPLOYMENT_NAME -n $NS"
echo "  kubectl get pods -n $NS -l app=pre-condition-test"
echo ""
echo "To use in pre-conditions:"
echo "  appReadinessChecks:"
echo "    - \"$NS.$DEPLOYMENT_NAME\""
echo ""
echo "To clean up:"
echo "  kubectl delete namespace $NS"
echo ""
