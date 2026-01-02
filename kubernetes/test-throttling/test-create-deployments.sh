#!/usr/bin/env bash

# Script: kubernetes/test-create-deployments.sh
# Purpose: Create test workloads for adaptive throttling testing

set -e

# === PARAMETER VALIDATION ===

if [[ -z "$1" ]]; then
    echo "Error: Namespace parameter is required"
    echo "Usage: IMAGE=nginx REPLICAS=3 NUMBER=10 TYPE=deployment $0 <namespace>"
    exit 1
fi

if [[ -z "$IMAGE" ]]; then
    echo "Error: IMAGE environment variable is required"
    echo "Usage: IMAGE=nginx REPLICAS=3 NUMBER=10 TYPE=deployment $0 <namespace>"
    exit 1
fi

if [[ -z "$REPLICAS" ]]; then
    echo "Error: REPLICAS environment variable is required"
    echo "Usage: IMAGE=nginx REPLICAS=3 NUMBER=10 TYPE=deployment $0 <namespace>"
    exit 1
fi

if [[ -z "$NUMBER" ]]; then
    echo "Error: NUMBER environment variable is required"
    echo "Usage: IMAGE=nginx REPLICAS=3 NUMBER=10 TYPE=deployment $0 <namespace>"
    exit 1
fi

if [[ -z "$TYPE" ]]; then
    echo "Error: TYPE environment variable is required"
    echo "Usage: IMAGE=nginx REPLICAS=3 NUMBER=10 TYPE=deployment $0 <namespace>"
    exit 1
fi

if [[ "$TYPE" != "deployment" && "$TYPE" != "statefulset" ]]; then
    echo "Error: TYPE must be 'deployment' or 'statefulset'"
    echo "Got: $TYPE"
    exit 1
fi

NS="$1"

echo "====================================="
echo "Creating test workloads"
echo "====================================="
echo "Namespace:       $NS"
echo "Type:            $TYPE"
echo "Image:           $IMAGE"
echo "Replicas/each:   $REPLICAS"
echo "Count:           $NUMBER"
echo "Total pods:      $((NUMBER * REPLICAS))"
echo "====================================="

# === CREATE NAMESPACE ===

echo "***"
echo "Creating namespace: $NS"
kubectl create namespace "$NS" 2>/dev/null || echo "Namespace $NS already exists"

# === CREATE WORKLOADS ===

if [[ "$TYPE" == "deployment" ]]; then
    echo ""
    echo "Creating $NUMBER deployments..."

    for i in $(seq 1 "$NUMBER"); do
        WORKLOAD_NAME="$NS-deploy-$i"
        echo "  [$i/$NUMBER] Creating deployment: $WORKLOAD_NAME"

        kubectl create deployment "$WORKLOAD_NAME" \
            --image="$IMAGE" \
            --replicas="$REPLICAS" \
            -n "$NS" \
            --dry-run=client -o yaml | \
        kubectl label -f - app=test-throttle --overwrite --dry-run=client --local -o yaml | \
        kubectl apply -f -
    done

elif [[ "$TYPE" == "statefulset" ]]; then
    echo ""
    echo "Creating $NUMBER statefulsets..."

    for i in $(seq 1 "$NUMBER"); do
        WORKLOAD_NAME="$NS-sts-$i"
        echo "  [$i/$NUMBER] Creating statefulset: $WORKLOAD_NAME"

        cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: $WORKLOAD_NAME
  namespace: $NS
  labels:
    app: test-throttle
spec:
  selector:
    matchLabels:
      app: test-throttle
      workload: $WORKLOAD_NAME
  serviceName: "nginx"
  replicas: $REPLICAS
  template:
    metadata:
      labels:
        app: test-throttle
        workload: $WORKLOAD_NAME
    spec:
      containers:
      - name: nginx
        image: $IMAGE
EOF
    done
fi

# === SUMMARY ===

echo ""
echo "====================================="
echo "✅ Test workloads created successfully!"
echo "====================================="
echo ""
echo "Summary:"
kubectl get deployments,statefulsets -n "$NS" -l app=test-throttle
echo ""
echo "To test adaptive throttling:"
echo "1. Freeze: kubectl apply -f <policy-freeze.yaml>"
echo "2. Resume: kubectl apply -f <policy-resume-with-throttling.yaml>"
echo "3. Monitor: kubectl get namespacelifecyclepolicy <name> -o yaml"
echo ""
