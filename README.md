# ArgoCD auto Attach Service

This supervisor service can be used to automatically add workload clusters to ArgoCD as they are deployed or after the fact . This is currently paired with this [ArgoCD service](https://vsphere-tmm.github.io/Supervisor-Services/#argocd-operator).


## How it works

ArgoCD currently runs centralized in the supervisor cluster. When deploying workload clusters by default these clusters are not registered back to ArgoCD. This supervsior service handles that by introducing a controller that has a new CRD called `ArgoCluster`. This CRD can be created alongside any cluster and it will register that cluster with the argocd instance in the specified namespace. Behind the scenes this controller will create the secret necessary to register the cluster to argo.

## Install

1. login into VCenter and go to the worload management->services page
2. add a new service and upload the argo-attach.yml
3. add any additional values that are needed. see the values section below.
4. install


## Values
| Field               | Value         | Description |
|--------------------|---------------|-------------|
| `resync_period`     | `60`          | Resync period in seconds |
| `namespace`         | `""`          | namespace to deploy into, this is filled by the supervisor do not edit|
| `blocked_namespaces`| `[""]`        | Namespaces that should not be allowed |

## AirGap Install

1. relocate the image bundle to you repository, grab the latest imaeg bundle from the `argo-atatch.yml`

```bash
imgpkg copy -b <latest bundle from argo-attach.yml> --to-repo your-repo.com/argocd-auto-attach
```

2. replace the image bundle in the `argo-atatch.yml` with your new path to the image bundle. the SHA should still be the same so all you should need to replace is the `ghcr.io/warroyo/argocd-auto-attach` with your repo and path.

3. follow the steps to install in the previosu step. 


## Usage

Once the service is installed and also ArgoCD is installed. Follow these steps

1. update the yaml in the `examples/argoCluster.yml` with your details
2. `kubectl apply -f examples/argoCluster.yml`


## Sample CRD

### ArgoCluster

this is a sample CRD that handles adding clusters to ArgoCD

```yaml
apiVersion: field.vmware.com/v1
kind: ArgoCluster
metadata:
  name: sample-cluster
spec:
  clusterName: "sample-cluster"
  argoNamespace: "default"
  clusterLabels:
    test: "test"
  project: testing
```
## Development
 

### Releasing

```bash
export VERSION=1.0.0
make release
```
