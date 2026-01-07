#!/usr/bin/env bash

# Script: kubernetes/test-throttling/trigger-cpu-load.sh
# Purpose: Generate real CPU load to test adaptive throttling (🐌)

NS="cpu-stress-ns"
REPLICAS=10 # Node sayınıza göre bu sayıyı artırabilirsiniz

echo "🚀 Creating CPU load in namespace: $NS"
kubectl create namespace "$NS" 2>/dev/null || true

cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cpu-load-generator
  namespace: $NS
spec:
  replicas: $REPLICAS
  selector:
    matchLabels:
      app: stressor
  template:
    metadata:
      labels:
        app: stressor
    spec:
      containers:
      - name: busy-loop
        image: busybox
        command: ["/bin/sh", "-c", "while true; do : ; done"]
        resources:
          requests:
            cpu: "1" # Her 1 core CPU çekirdeği tüketir
EOF

echo "✅ CPU load generator deployed."
echo "Wait 30-60 seconds for metrics to reflect in Kubernetes."
echo ""
echo "Monitor with: kubectl top nodes"
echo "Clean up with: kubectl delete namespace $NS"
