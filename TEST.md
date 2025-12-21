kubectl create namespace test-ns
kubectl create deployment nginx --image=nginx --replicas=4 -n test-ns
kubectl label deployment nginx app=test -n test-ns --overwrite


# Start Kubernetes by Kind
bash kubernetes/kind/start-3nodes.sh



# Drain node
kubectl drain dev-worker2 --ignore-daemonsets --delete-emptydir-data

# Uncordon node
kubectl uncordon dev-worker2