#!/usr/bin/env bash

export IMG=ghcr.io/baturorkun/app-lifecycle-kubernetes-operator:latest

make deploy IMG=$IMG

kustomize build config/default > dist/install.yaml