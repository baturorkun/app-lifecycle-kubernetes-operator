#!/usr/bin/env bash

# Script: kubernetes/test-throttling/pre-condition.sh
# Purpose: Create a namespace and deployment for testing pre-conditions feature
#          The deployment waits READY_DELAY seconds before becoming ready
#
# Usage: READY_DELAY=30 ./pre-condition.sh
#        Default: 60 seconds

set -e

NS="pre-condition"
DEPLOYMENT_NAME="slow-app"
READY_DELAY="${READY_DELAY:-60}"  # Default 60 seconds if not specified

echo "====================================="
echo "Deleting Pre-Condition Test Resources"
echo "====================================="

kubectl delete namespace "$NS" 2>/dev/null || echo "Namespace $NS NOT exists"


echo "====================================="
echo "Creating Pre-Condition Test Resources"
echo "====================================="
echo "Namespace:       $NS"
echo "Deployment:      $DEPLOYMENT_NAME"
echo "Ready Delay:     $READY_DELAY seconds"
echo "====================================="

# === CREATE NAMESPACE ===

echo ""
echo "Creating namespace: $NS"
kubectl create namespace "$NS" 2>/dev/null || echo "Namespace $NS already exists"

# === CREATE DEPLOYMENT ===

echo ""
echo "Creating deployment: $DEPLOYMENT_NAME"
echo "This deployment will wait $READY_DELAY seconds before becoming ready..."

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
        image: busybox:latest
        # Sleep for $READY_DELAY seconds, then create /tmp/ready file, then sleep forever
        command: ["/bin/sh", "-c", "echo 'Starting slow-app, will be ready in $READY_DELAY seconds...'; sleep $READY_DELAY; touch /tmp/ready; echo 'Ready!'; sleep infinity"]
        # Readiness probe: checks if /tmp/ready file exists
        # This simulates an app that takes time to become ready
        readinessProbe:
          exec:
            command:
              - cat
              - /tmp/ready
          initialDelaySeconds: 1
          periodSeconds: 2
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
echo "  - Pod starts immediately (busybox container runs)"
echo "  - Container sleeps for $READY_DELAY seconds"
echo "  - After $READY_DELAY seconds, creates /tmp/ready file"
echo "  - Readiness probe checks for /tmp/ready file"
echo "  - Deployment becomes ready after ~$READY_DELAY seconds"
echo ""
echo "To check readiness status:"
echo "  kubectl get deployment $DEPLOYMENT_NAME -n $NS"
echo "  kubectl get pods -n $NS -l app=pre-condition-test"
echo ""
echo "To use in pre-conditions:"
echo "  appReadinessChecks:"
echo "    - \"$NS.$DEPLOYMENT_NAME\""
echo ""
echo "To run with different delay:"
echo "  READY_DELAY=30 ./pre-condition.sh"
echo ""
echo "To clean up:"
echo "  kubectl delete namespace $NS"
echo ""
