# Example Usings

IMAGE=nginx REPLICAS=1 NUMBER=10 TYPE=deployment ./kubernetes/test-create-deployments.sh test1-ns
IMAGE=nginx REPLICAS=1 NUMBER=10 TYPE=deployment ./kubernetes/test-create-deployments.sh test2-ns
IMAGE=nginx REPLICAS=1 NUMBER=10 TYPE=deployment ./kubernetes/test-create-deployments.sh test3-ns
IMAGE=nginx REPLICAS=1 NUMBER=10 TYPE=deployment ./kubernetes/test-create-deployments.sh test4-ns
IMAGE=nginx REPLICAS=1 NUMBER=10 TYPE=deployment ./kubernetes/test-create-deployments.sh test5-ns


# Delete All CRs
kubectl delete namespacelifecyclepolicy --all

# Run
NAMESPACES="test1-ns test2-ns test3-ns test4-ns test5-ns test6-ns test7-ns" ./create-throttling-policies.sh