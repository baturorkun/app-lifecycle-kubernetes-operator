#!/usr/bin/env bash

IMAGE=nginx REPLICAS=1 NUMBER=10 TYPE=deployment ./test-create-deployments.sh test1
IMAGE=nginx REPLICAS=1 NUMBER=10 TYPE=deployment ./test-create-deployments.sh test2
IMAGE=nginx REPLICAS=1 NUMBER=10 TYPE=deployment ./test-create-deployments.sh test3
IMAGE=nginx REPLICAS=1 NUMBER=10 TYPE=deployment ./test-create-deployments.sh test4
IMAGE=nginx REPLICAS=1 NUMBER=10 TYPE=deployment ./test-create-deployments.sh test5

#NAMESPACES="test1 test2 test3 test4 test5" ./create-throttling-policies.sh

NAMESPACES="test1 test2 test3 test4 test5" ./create-throttling-policies.sh

# Add cross-namespace pre-condition to policy-test2:
# test2 must wait for test1-deploy-1 in test1 to be ready before resuming.
# This verifies that priority ordering + pre-conditions work correctly:
# policy-test1 (priority 1) resumes first → test1-deploy-1 becomes ready →
# policy-test2 (priority 2) pre-condition passes → test2 resumes.
kubectl patch namespacelifecyclepolicy policy-test2 -n default --type='merge' -p='
spec:
  preConditions:
    enabled: true
    blockPriorityChain: true
    appReadinessChecks:
      - "test1.test1-deploy-1"
    checkInterval: 5
    timeoutSeconds: 600
'

# Add cross-namespace pre-condition to policy-test3:
# test3 must wait for test2-deploy-1 in test2 to be ready before resuming.
kubectl patch namespacelifecyclepolicy policy-test3 -n default --type='merge' -p='
spec:
  preConditions:
    enabled: true
    blockPriorityChain: true
    appReadinessChecks:
      - "test2.test2-deploy-1"
    checkInterval: 5
    timeoutSeconds: 600
'

#podman restart  dev-worker dev-worker2