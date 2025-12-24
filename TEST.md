# Start Kubernetes by Kind
bash kubernetes/kind/start-3nodes.sh


# Create Deployment

kubectl create namespace test-ns
kubectl create deployment nginx --image=nginx --replicas=4 -n test-ns
kubectl label deployment nginx app=test -n test-ns --overwrite


# Create StatefulSet
cat <<EOF | kubectl apply -n test-ns -f -
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: web
  labels:
    app: test
spec:
  selector:
    matchLabels:
      app: test
  serviceName: "nginx"
  replicas: 3
  template:
    metadata:
      labels:
        app: test
    spec:
      containers:
      - name: nginx
        image: nginx

EOF


# Drain node
kubectl drain dev-worker2 --ignore-daemonsets --delete-emptydir-data

# Uncordon node
kubectl uncordon dev-worker2

# Trigger Reconcile
kubectl patch namespacelifecyclepolicy nlp --type=merge -p '{"spec":{"action":"Freeze","operationId":"op-020"}}'
