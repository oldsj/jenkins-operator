# Installation

This document describes installation procedure for **jenkins-operator**.

## Requirements
 
To run **jenkins-operator**, you will need:
- running Kubernetes cluster
- kubectl

## Configure Custom Resource Definition 

Install Jenkins Custom Resource Definition:

```bash
kubectl create -f deploy/crds/virtuslab_v1alpha1_jenkins_crd.yaml
```

## Deploy jenkins-operator

Deploy **jenkins-operator** with RBAC (it may take some time):

```bash
kubectl create -f deploy/service_account.yaml
kubectl create -f deploy/role.yaml
kubectl create -f deploy/role_binding.yaml
kubectl create -f deploy/operator.yaml
```

Watch **jenkins-operator** instance being created:

```bash
kubectl get pods -w
```

Now **jenkins-operator** should be up and running in `default` namespace.


