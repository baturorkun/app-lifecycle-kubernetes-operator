#!/usr/bin/env bash

if [[ $1 == "" ]]; then
    echo "Error: Version is missing. The version must be like that v0.1.0"
    exit
fi

export IMG=ghcr.io/baturorkun/app-lifecycle-kubernetes-operator:$1

make deploy IMG=$IMG

kustomize build config/default > dist/install.yaml

git tag "$1"

git push origin "$1"
