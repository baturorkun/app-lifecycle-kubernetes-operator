#!/usr/bin/env bash

echo "Create Cluster"
kind create cluster --name dev --config kind-3nodes_v.yaml


echo "Set Worker Label"
kubectl label node dev-worker node-role.kubernetes.io/worker= --overwrite
kubectl label node dev-worker2 node-role.kubernetes.io/worker= --overwrite
