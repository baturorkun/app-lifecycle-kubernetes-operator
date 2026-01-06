#!/usr/bin/env bash

IMAGE=nginx REPLICAS=1 NUMBER=20 TYPE=deployment ./test-create-deployments.sh test1
IMAGE=nginx REPLICAS=1 NUMBER=20 TYPE=deployment ./test-create-deployments.sh test2
IMAGE=nginx REPLICAS=1 NUMBER=20 TYPE=deployment ./test-create-deployments.sh test3
IMAGE=nginx REPLICAS=1 NUMBER=20 TYPE=deployment ./test-create-deployments.sh test4
IMAGE=nginx REPLICAS=1 NUMBER=20 TYPE=deployment ./test-create-deployments.sh test5

NAMESPACES="test1 test2 test3 test4 test5" ./create-throttling-policies.sh

podman restart  dev-worker dev-worker2