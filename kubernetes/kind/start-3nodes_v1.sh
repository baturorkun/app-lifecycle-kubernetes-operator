#!/usr/bin/env bash

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"

echo "Create Cluster"
kind create cluster --name dev --config "${SCRIPT_DIR}/kind-3nodes_v1.yaml"


echo "Set Worker Label"
kubectl label node dev-worker node-role.kubernetes.io/worker= --overwrite
kubectl label node dev-worker2 node-role.kubernetes.io/worker= --overwrite
