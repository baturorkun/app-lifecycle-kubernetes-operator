#!/usr/bin/env bash

kind create cluster --name dev --config kind-3nodes.yaml

kubectl label node dev-worker node-role.kubernetes.io/worker= --overwrite

kubectl label node dev-worker2 node-role.kubernetes.io/worker= --overwrite
