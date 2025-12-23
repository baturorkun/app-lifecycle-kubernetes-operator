#!/usr/bin/env bash

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"

echo "Create Cluster"
kind create cluster --name dev --config "${SCRIPT_DIR}/kind-3nodes_v2.yaml"
