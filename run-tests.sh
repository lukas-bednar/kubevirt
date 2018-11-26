#!/usr/bin/env bash

#./_out/tests/tests.test \
#    -kubeconfig=openshift-master.kubeconfig \
#    -cluster-type=ocp \
#    -path-to-testing-infra-manifests=/home/lbednar/work/kubevirt-org/go/src/kubevirt.io/kubevirt/_out/manifests/testing \
#    -deploy-testing-infra

#docker run \
#    -v /tmp/kubevirt-tests-data:/home/kubevirt-tests/data:rw,z --rm \
#    kubevirt/tests:latest \
#        --kubeconfig=data/openshift-master.kubeconfig \
#        --docker-tag=latest \
#        --docker-prefix=docker.io/kubevirt \
#        --test.timeout 60m \
#        --deploy-testing-infra \
#        --installed-namespace=kube-system \
#        --path-to-testing-infra-manifests=data/manifests \
#        --cluster-type=ocp

./_out/tests/tests.test \
    -kubeconfig=cluster/k8s-1.10.4/.kubeconfig \
    -path-to-testing-infra-manifests=_out/manifests/testing \
    -deploy-testing-infra \
    -docker-tag=latest \
    -docker-prefix=docker.io/kubevirt \
    -installed-namespace=kube-system \
    -test.timeout 60m
