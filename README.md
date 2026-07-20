# ArgoCD Auto Attach Service

![Release Status](https://github.com/warroyo/argocd-attach-service/actions/workflows/build-release.yml/badge.svg)

Supervisor service that auto-registers workload clusters and namespaces with ArgoCD, whether at deploy time or after the fact. Pairs with the [ArgoCD supervisor service](https://vsphere-tmm.github.io/Supervisor-Services/#argocd-operator).

> **VCF 9.1+**: Version `3.0.7` or higher is required.

## How it works

ArgoCD runs centralized in the supervisor cluster, but workload clusters don't register themselves. This controller adds CRDs to handle that: `ArgoCluster`, `ArgoNamespace`, and `RemoteNamespace` (gated by `ArgoAttachAuthority`).

**ArgoCluster**: reads the kubeconfig secret created when a VKS cluster is provisioned and creates the ArgoCD cluster secret in the specified namespace.

**ArgoNamespace**: registers a supervisor namespace as an ArgoCD target. Deployed *into* the target namespace. Either uses an existing service account you point it at, or creates one (`argo-attach-sa`) with an `edit` RoleBinding, then writes the cluster secret into the ArgoCD namespace.

**RemoteNamespace**: the inverse of `ArgoNamespace`: deployed *into the ArgoCD namespace*, pointing at a remote target namespace. This lets ArgoCD itself manage namespace attachment via GitOps; no access to the target namespace needed. Only honored in namespaces designated by an `ArgoAttachAuthority`.

**ArgoAttachAuthority**: cluster-scoped designation, creatable only by the SSO groups configured at install. Marks a namespace as hosting a platform-approved ArgoCD and bounds which targets it may attach.

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

2. In `argo-attach.yml`, replace `ghcr.io/warroyo/argocd-auto-attach` with your registry path. SHA stays the same; only the registry prefix changes.

3. Follow the UI steps above.

## Values

| Field                | Default | Description |
|---------------------|---------|-------------|
| `resync_period`      | `"60"`  | Periodic reconcile interval in seconds |
| `namespace`          | `""`    | Namespace to deploy into (filled by the supervisor, do not edit) |
| `blocked_namespaces` | `[""]`  | Namespaces that cannot be used as the `argoNamespace` target in any CR, or as a `RemoteNamespace` target |
| `admin_sso_groups`   | `["sso:Administrators@vsphere.local"]` | SSO groups allowed to create `ArgoAttachAuthority` resources. Verify the exact subject string in your environment (`kubectl get clusterrolebindings -o yaml \| grep sso:`) |

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

### RemoteNamespace

Attach namespaces *from* the ArgoCD namespace, so the whole flow is GitOps: terraform/vCenter creates the supervisor namespace, and a `RemoteNamespace` file in Git (synced by ArgoCD into its own namespace) does the attach. Deleting the file detaches: the finalizer tears down everything the attach created.

**Onboarding an ArgoCD instance (one-time, supervisor admin):**

1. Designate the instance's namespace with an `ArgoAttachAuthority` (cluster-scoped; only members of `admin_sso_groups` can create these):

```yaml
apiVersion: field.vmware.com/v1
kind: ArgoAttachAuthority
metadata:
  name: platform
spec:
  namespace: "argocd"          # the ArgoCD instance's namespace
  allowedTargets: ["*"]        # or bound it, e.g. ["apps-*", "team-a"]
```

2. Grant the instance's ArgoCD service account the ability to manage `RemoteNamespace` resources in that namespace (the `remotenamespace-edit` ClusterRole ships with the service, deliberately not aggregated into `edit`):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: argocd-remotenamespace-edit
  namespace: argocd
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: remotenamespace-edit
subjects:
- kind: ServiceAccount
  name: argocd-application-controller   # your instance's app controller SA
  namespace: argocd
```

**Attaching a namespace (per namespace, via Git):**

```yaml
apiVersion: field.vmware.com/v1
kind: RemoteNamespace
metadata:
  name: attach-team-a
  namespace: argocd                     # must be a designated namespace
spec:
  targetNamespace: team-a               # the remote namespace to attach
  project: default                      # ArgoCD project (the cluster secret is project-scoped)
  clusterLabels:
    env: production                     # optional labels applied to the ArgoCD cluster secret
```

The controller refuses to attach: targets outside the authority's `allowedTargets`; `kube-system`/`kube-public`/`kube-node-lease`; anything prefixed `vmware-system-` or `svc-`; any designated authority namespace or the controller's own namespace; and anything in `blocked_namespaces`. If the target namespace is deleted and recreated under the same name, the CR goes `Failed` instead of silently re-attaching. Delete and re-create the CR to re-attach.

The service account is named `argo-attach-<argocd-namespace>`, so a local `ArgoNamespace` attach and remote attaches from multiple ArgoCD instances can coexist in one target namespace. There is no `serviceAccount` option on `RemoteNamespace`: allowing a remote requester to mint tokens for pre-existing service accounts in namespaces they can't access would be a privilege-escalation vector.

**GitOps pattern:** keep a `namespaces/` directory (one `RemoteNamespace` per file) in a platform repo, synced by an Application with prune enabled. Adding a namespace is a PR; deleting the file detaches. Surface attach failures in the ArgoCD UI with a health check in `argocd-cm`:

```yaml
resource.customizations.health.field.vmware.com_RemoteNamespace: |
  hs = {}
  if obj.status ~= nil and obj.status.state == "Ready" then
    hs.status = "Healthy"
    hs.message = obj.status.message
  elseif obj.status ~= nil and obj.status.state == "Failed" then
    hs.status = "Degraded"
    hs.message = obj.status.message
  else
    hs.status = "Progressing"
  end
  return hs
```

**Security model (read this before designating authorities):**

- Designating a namespace with an `ArgoAttachAuthority` grants that namespace's members and Applications the power to obtain `edit` on any namespace matching `allowedTargets`. Treat it like handing out a cluster role.
- Anyone who can write into a designated namespace (directly, or via an ArgoCD Application whose project allows that destination) can attach namespaces. Only platform-controlled projects should be able to deploy into designated namespaces.
- Membership on an ArgoCD namespace is equivalent to holding the credentials of every namespace it has attached (the cluster secrets live there).
- The controller's deployment, values, and the `argoattachauthority-admin` binding are protected surface: whoever can edit them can mint authorities.
- Local `ArgoNamespace` and remote attaches use distinct service account names (`argo-attach-sa` vs `argo-attach-<argocd-namespace>`), so mixing them on one target namespace is safe. Avoid multiple `RemoteNamespace` CRs in the *same* ArgoCD namespace pointing at the *same* target: they would share one SA and cluster secret, and deleting either tears both down.
- Cluster secret tokens are long-lived legacy service account tokens; rotating to short-lived TokenRequest tokens is planned. Until then, treat detach (which deletes the token) as the revocation mechanism.

## CRD Status

`ArgoCluster`, `ArgoNamespace`, and `RemoteNamespace` track reconciliation state in `.status`:

| State     | Description |
|-----------|-------------|
| `Pending` | Finalizer added, provisioning not yet run |
| `Ready`   | Successfully registered with ArgoCD |
| `Failed`  | Reconciliation error, see `message` for details |

```bash
kubectl get argocluster <name> -n <namespace> -o jsonpath='{.status}'
kubectl get argonamespace <name> -n <namespace> -o jsonpath='{.status}'
kubectl get remotenamespace <name> -n <namespace> -o jsonpath='{.status}'
```

`RemoteNamespace` additionally records `attachedNamespaceUID` and `serviceAccount` on successful provision. Cleanup only deletes what this inventory says was created, and a recreated target namespace (changed UID) is detected instead of silently re-attached.

## Development

### Testing

No supervisor or ArgoCD needed. The controller's output is plain Kubernetes objects, so the whole suite runs against a local [kind](https://kind.sigs.k8s.io/) cluster.

```bash
make test-unit   # go vet + unit tests for the gate/naming functions
make test-e2e    # full kind-based suite (requires kind, kubectl, ytt, go)
```

The e2e suite runs the controller via `go run`, but authenticated **as the `argoattach` service account** (token minted per run). Every API call is authorized against the real ClusterRole from `config/deploy.yml`, so a missing RBAC rule fails the suite with `Forbidden` just like it would in a real deployment. It covers all reconcile gates, the cleanup guard, local/remote coexistence, namespace-recreation (UID) pinning, detach, the ArgoCluster kubeconfig flow (the kind cluster mocks its own workload cluster), blocked-namespace enforcement, and the RBAC boundaries via impersonation.

Debugging: `test/e2e.sh --keep` leaves the kind cluster and controller running on failure; the harness prints the controller log on any failed assertion. Both jobs run in GitHub Actions on every PR (`.github/workflows/test.yml`).

Still requires a real supervisor to verify: the exact SSO subject strings for `admin_sso_groups`, supervisor admin role behavior, the `?context=<ns>` server URL semantics, and the in-cluster startup paths.

### Releasing

Push a `v*` tag and GitHub Actions does the rest.

```bash
git tag v1.0.0
git push origin v1.0.0
```

The pipeline:

1. Builds and pushes the controller image to `ghcr.io/warroyo/argocd-attach-service/controller:<version>`
2. Runs `kctrl package release` to build the Carvel package
3. Assembles `argo-attach.yml` from the generated package metadata and spec
4. Creates a GitHub Release with `argo-attach.yml` attached and auto-generated release notes

`argo-attach.yml` is what you upload as a supervisor service. Release steps only run on tag pushes; `workflow_dispatch` builds but doesn't push or publish.

**Local release:**

```bash
export VERSION=1.0.0
make build-controller
make release-controller
make release
```
