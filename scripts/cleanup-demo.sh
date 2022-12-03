#!/usr/bin/env bash

if [ "$1" == "local" ]; then
  helm --kube-context microk8s delete -n nginx nginx
  helm --kube-context minikube delete -n nginx nginx
  helm --kube-context microk8s delete -n nginx-staging nginx
fi

if [ "$1" == "work" ]; then
  helm --kube-context docker-decktop delete -n nginx nginx
  helm --kube-context minikube delete -n nginx nginx
  helm --kube-context docker-desktop delete -n nginx-staging nginx
fi
