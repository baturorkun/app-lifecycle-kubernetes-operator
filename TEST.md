kubectl create namespace test-ns
kubectl create deployment nginx --image=nginx --replicas=3 -n test-ns
kubectl label deployment nginx app=test -n test-ns --overwrite


