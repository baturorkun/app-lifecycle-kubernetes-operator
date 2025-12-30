# Example Usings

IMAGE=nginx REPLICAS=1 NUMBER=10 TYPE=deployment ./kubernetes/test-create-deployments.sh test1-ns
IMAGE=nginx REPLICAS=1 NUMBER=10 TYPE=deployment ./kubernetes/test-create-deployments.sh test2-ns
IMAGE=nginx REPLICAS=1 NUMBER=10 TYPE=deployment ./kubernetes/test-create-deployments.sh test3-ns



NAMESPACES="test1-ns test2-ns test3-ns" ./create-throttling-policies.sh