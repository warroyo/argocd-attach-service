# ArgoCD Auto Attach Service

![Release Status](https://github.com/warroyo/argocd-attach-service/actions/workflows/build-release.yml/badge.svg)

Supervisor service that auto-registers workload clusters and namespaces with ArgoCD — at deploy time or after the fact. Pairs with the [ArgoCD supervisor service](https://vsphere-tmm.github.io/Supervisor-Services/#argocd-operator).

> **VCF 9.1+**: Version `3.0.7` or higher is required.

## How it works

ArgoCD runs centralized in the supervisor cluster, but workload clusters don't register themselves. This controller adds two CRDs to handle that — `ArgoCluster` and `ArgoNamespace`.

**ArgoCluster** — reads the kubeconfig secret created when a VKS cluster is provisioned and creates the ArgoCD cluster secret in the specified namespace.

**ArgoNamespace** — registers a supervisor namespace as an ArgoCD target. Either uses an existing service account you point it at, or creates one (`argo-attach-sa`) with an `edit` RoleBinding, then writes the cluster secret into the ArgoCD namespace.

## Prerequisites

- VCF supervisor cluster
- [ArgoCD supervisor service](https://vsphere-tmm.github.io/Supervisor-Services/#argocd-operator) installed

## Install

### UI

1. Log into vCenter and navigate to **Workload Management → Services**
2. Click **Add Service** and upload `argo-attach.yml` from the [latest release](https://github.com/warroyo/argocd-attach-service/releases)
3. Configure values as needed (see [Values](#values) below)
4. Click **Install**

### AirGap

1. Relocate the image bundle to your registry:

```bash
imgpkg copy -b <bundle-ref-from-argo-attach.yml> --to-repo your-repo.example.com/argocd-auto-attach
```

2. In `argo-attach.yml`, replace `ghcr.io/warroyo/argocd-auto-attach` with your registry path. SHA stays the same — only the registry prefix changes.

3. Follow the UI steps above.

## Values

| Field                | Default | Description |
|---------------------|---------|-------------|
| `resync_period`      | `"60"`  | Periodic reconcile interval in seconds |
| `namespace`          | `""`    | Namespace to deploy into — filled by the supervisor, do not edit |
| `blocked_namespaces` | `[""]`  | Namespaces that cannot be used as the `argoNamespace` target in any CR |

## Usage

### ArgoCluster

The kubeconfig secret is automatically created when a VKS cluster is provisioned.

1. Update `examples/argoCluster.yml` with your cluster details
2. Apply it in the same namespace as the kubeconfig secret:

```bash
kubectl apply -f examples/argoCluster.yml
```

```yaml
apiVersion: field.vmware.com/v1
kind: ArgoCluster
metadata:
  name: sample-cluster
  namespace: my-namespace
spec:
  clusterName: "sample-cluster"       # must match the kubeconfig secret prefix and cluster name in kubeconfig
  argoNamespace: "argocd"             # namespace where ArgoCD is installed
  project: default                    # ArgoCD project to register under
  clusterLabels:
    env: production                   # optional labels applied to the ArgoCD cluster secret
```

### ArgoNamespace

Apply this in the supervisor namespace you want to register with ArgoCD.

1. Update `examples/argoNs.yml` with your details
2. Apply it in the target namespace:

```bash
kubectl apply -f examples/argoNs.yml
```

```yaml
apiVersion: field.vmware.com/v1
kind: ArgoNamespace
metadata:
  name: sample-ns
  namespace: my-supervisor-namespace
spec:
  argoNamespace: "argocd"             # namespace where ArgoCD is installed
  project: default                    # ArgoCD project to register under
  serviceAccount: ""                  # leave empty to auto-create, or provide an existing SA name
  clusterLabels:
    env: production                   # optional labels applied to the ArgoCD cluster secret
```

**Service account behavior:**
- If `serviceAccount` is empty, the controller creates `argo-attach-sa` with an `edit` RoleBinding and generates a token secret.
- If `serviceAccount` is set, the controller uses that SA and creates a token secret for it. The SA must already exist in the namespace.

## CRD Status

Both CRDs track reconciliation state in `.status`:

| State     | Description |
|-----------|-------------|
| `Pending` | Finalizer added, provisioning not yet run |
| `Ready`   | Successfully registered with ArgoCD |
| `Failed`  | Reconciliation error — see `message` for details |

```bash
kubectl get argocluster <name> -n <namespace> -o jsonpath='{.status}'
kubectl get argonamespace <name> -n <namespace> -o jsonpath='{.status}'
```

## Development

### Releasing

Push a `v*` tag — GitHub Actions does the rest.

```bash
git tag v1.0.0
git push origin v1.0.0
```

The pipeline:

1. Builds and pushes the controller image to `ghcr.io/warroyo/argocd-attach-service/controller:<version>`
2. Runs `kctrl package release` to build the Carvel package
3. Assembles `argo-attach.yml` from the generated package metadata and spec
4. Creates a GitHub Release with `argo-attach.yml` attached and auto-generated release notes

`argo-attach.yml` is what you upload as a supervisor service. Release steps only run on tag pushes — `workflow_dispatch` builds but doesn't push or publish.

**Local release:**

```bash
export VERSION=1.0.0
make build-controller
make release-controller
make release
```
