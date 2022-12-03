#!/usr/bin/env bash

if [ "$1" == "local" ]; then
  go run cmd/worker/main.go -m prod1=microk8s -m prod2=minikube -m staging1=microk8s
fi

if [ "$1" == "work" ]; then
  go run cmd/worker/main.go -m prod1=docker-desktop -m prod2=minikube -m staging1=docker-desktop
fi