#!/usr/bin/env bash
# End-to-end test suite for the argocd-attach-service controller.
#
# Runs against a local kind cluster — no supervisor or ArgoCD required. The
# controller runs via `go run` but authenticates AS the argoattach service
# account (token minted per run), so every API call is authorized against the
# real ClusterRole from config/deploy.yml: RBAC drift fails the suite with
# Forbidden, exactly as it would in a real deployment.
#
# Usage: test/e2e.sh [--keep]
#   --keep  leave the kind cluster and controller running on exit (debugging)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURES="$REPO_ROOT/test/fixtures"
CLUSTER_NAME="argo-attach-e2e"
SYSTEM_NS="argo-attach-e2e-system"
RESYNC_SECONDS=10
WAIT_TIMEOUT=${WAIT_TIMEOUT:-120}
KEEP=false
[[ "${1:-}" == "--keep" ]] && KEEP=true

WORKDIR="$(mktemp -d)"
ADMIN_KUBECONFIG="$WORKDIR/admin.kubeconfig"
CONTROLLER_KUBECONFIG="$WORKDIR/controller.kubeconfig"
CONTROLLER_LOG="$WORKDIR/controller.log"
CONTROLLER_PID=""

# every kubectl in this script uses the dedicated admin kubeconfig, never the
# developer's default context
k() { kubectl --kubeconfig "$ADMIN_KUBECONFIG" "$@"; }

log() { echo -e "\033[1;34m[e2e]\033[0m $*"; }
pass() { echo -e "\033[1;32m[PASS]\033[0m $*"; }

fail() {
	echo -e "\033[1;31m[FAIL]\033[0m $*" >&2
	echo "--- controller log (last 100 lines) ---" >&2
	tail -n 100 "$CONTROLLER_LOG" 2>/dev/null >&2 || true
	echo "--- remotenamespaces ---" >&2
	k get remotenamespaces -A -o yaml 2>/dev/null >&2 || true
	exit 1
}

cleanup() {
	local code=$?
	if [[ -n "$CONTROLLER_PID" ]]; then
		kill "$CONTROLLER_PID" 2>/dev/null || true
		# go run spawns a child binary; kill the process group remnants
		pkill -P "$CONTROLLER_PID" 2>/dev/null || true
	fi
	if [[ "$KEEP" == "true" ]]; then
		log "--keep set: leaving cluster '$CLUSTER_NAME' up. kubeconfig: $ADMIN_KUBECONFIG"
	else
		kind delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
		rm -rf "$WORKDIR"
	fi
	exit $code
}
trap cleanup EXIT

# --- helpers -----------------------------------------------------------------

# wait_for_status <resource> <name> <namespace> <state> [substring]
wait_for_status() {
	local resource=$1 name=$2 ns=$3 want=$4 substr=${5:-}
	local deadline=$((SECONDS + WAIT_TIMEOUT))
	while ((SECONDS < deadline)); do
		local state
		state=$(k get "$resource" "$name" -n "$ns" -o jsonpath='{.status.state}' 2>/dev/null || true)
		if [[ "$state" == "$want" ]]; then
			if [[ -n "$substr" ]]; then
				local msg
				msg=$(k get "$resource" "$name" -n "$ns" -o jsonpath='{.status.message}' 2>/dev/null || true)
				[[ "$msg" == *"$substr"* ]] && return 0
			else
				return 0
			fi
		fi
		sleep 2
	done
	fail "$resource/$name in $ns never reached state=$want${substr:+ with message containing '$substr'} (last state: ${state:-<none>})"
}

# wait_gone <resource> <name> <namespace>
wait_gone() {
	local resource=$1 name=$2 ns=$3
	local deadline=$((SECONDS + WAIT_TIMEOUT))
	while ((SECONDS < deadline)); do
		k get "$resource" "$name" -n "$ns" >/dev/null 2>&1 || return 0
		sleep 2
	done
	fail "$resource/$name in $ns still exists after ${WAIT_TIMEOUT}s"
}

# wait_exists <resource> <name> <namespace>
wait_exists() {
	local resource=$1 name=$2 ns=$3
	local deadline=$((SECONDS + WAIT_TIMEOUT))
	while ((SECONDS < deadline)); do
		k get "$resource" "$name" -n "$ns" >/dev/null 2>&1 && return 0
		sleep 2
	done
	fail "$resource/$name in $ns never appeared"
}

assert_exists() { k get "$1" "$2" -n "$3" >/dev/null 2>&1 || fail "expected $1/$2 in $3 to exist"; }
assert_absent() { k get "$1" "$2" -n "$3" >/dev/null 2>&1 && fail "expected $1/$2 in $3 to be absent"; return 0; }

# assert_jsonpath <resource> <name> <ns> <jsonpath> <expected>
assert_jsonpath() {
	local got
	got=$(k get "$1" "$2" -n "$3" -o jsonpath="$4" 2>/dev/null || true)
	[[ "$got" == "$5" ]] || fail "$1/$2 $4 = '$got', expected '$5'"
}

# can_i <expected yes|no> <verb> <resource> [-n ns] --as ... [--as-group ...]
# retries briefly to tolerate ClusterRole aggregation lag
can_i() {
	local expected=$1; shift
	local deadline=$((SECONDS + 30)) got=""
	while ((SECONDS < deadline)); do
		got=$(k auth can-i "$@" 2>/dev/null || true)
		[[ "$got" == "$expected" ]] && return 0
		sleep 2
	done
	fail "auth can-i $* = '$got', expected '$expected'"
}

# --- setup -------------------------------------------------------------------

for dep in kind kubectl ytt go; do
	command -v "$dep" >/dev/null || { echo "missing dependency: $dep" >&2; exit 1; }
done

log "creating kind cluster '$CLUSTER_NAME'"
kind delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER_NAME" --kubeconfig "$ADMIN_KUBECONFIG" --wait 120s

log "installing CRDs"
k apply -f "$REPO_ROOT/controller/manifests/crd.yml"

log "rendering and applying deploy manifests (RBAC + deployment doc validation)"
k create namespace "$SYSTEM_NS"
ytt -f "$REPO_ROOT/config/deploy.yml" -f "$REPO_ROOT/config/values.yml" -f "$FIXTURES/values-e2e.yml" >"$WORKDIR/rendered.yml"
k apply -f "$WORKDIR/rendered.yml"
# the Deployment's image placeholder is only resolved at package build time; it
# can never pull here, so scale it away — the suite runs its own controller
k scale deployment argo-attach-controller -n "$SYSTEM_NS" --replicas=0

log "creating test namespaces"
for ns in argocd argocd-two tenant-ns team-a team-b team-c vmware-system-test blocked-ns-test; do
	k create namespace "$ns"
done

log "minting argoattach service account token and controller kubeconfig"
SERVER=$(k config view --raw -o jsonpath='{.clusters[0].cluster.server}')
CADATA=$(k config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
SA_TOKEN=$(k create token argoattach -n "$SYSTEM_NS" --duration=2h)
cat >"$CONTROLLER_KUBECONFIG" <<EOF
apiVersion: v1
kind: Config
clusters:
- name: e2e
  cluster:
    server: $SERVER
    certificate-authority-data: $CADATA
users:
- name: argoattach
  user:
    token: $SA_TOKEN
contexts:
- name: argoattach@e2e
  context:
    cluster: e2e
    user: argoattach
current-context: argoattach@e2e
EOF

log "starting controller as the argoattach service account (go run)"
(
	cd "$REPO_ROOT/controller"
	KUBECONFIG="$CONTROLLER_KUBECONFIG" POD_NAMESPACE="$SYSTEM_NS" \
		go run . --blocked-ns=blocked-ns-test --resync-period=$RESYNC_SECONDS \
		>"$CONTROLLER_LOG" 2>&1
) &
CONTROLLER_PID=$!
sleep 5
kill -0 "$CONTROLLER_PID" 2>/dev/null || fail "controller failed to start"

# --- case 10 first: pure RBAC, no controller interaction ----------------------

log "case 10: RBAC boundaries"
can_i yes create argoattachauthorities --as=test-admin --as-group="sso:Administrators@vsphere.local"
can_i no create argoattachauthorities --as=random-user
k apply -f "$FIXTURES/tenant-edit-rolebinding.yml"
can_i no create remotenamespaces -n tenant-ns --as=tenant-user
can_i yes create argonamespaces -n tenant-ns --as=tenant-user
pass "case 10: admin group can designate; tenants cannot; RemoteNamespace not aggregated into edit; ArgoNamespace still is"

# --- case 1: gate 1 + heal -----------------------------------------------------

log "case 1: undesignated namespace is refused, designation heals it"
cat <<EOF | k apply -f -
apiVersion: field.vmware.com/v1
kind: RemoteNamespace
metadata: {name: rn-heal, namespace: tenant-ns}
spec: {targetNamespace: team-c, project: default}
EOF
wait_for_status remotenamespace rn-heal tenant-ns Failed "not designated"
assert_absent serviceaccount argo-attach-tenant-ns team-c
cat <<EOF | k apply -f -
apiVersion: field.vmware.com/v1
kind: ArgoAttachAuthority
metadata: {name: tenant}
spec: {namespace: tenant-ns, allowedTargets: ["team-c"]}
EOF
wait_for_status remotenamespace rn-heal tenant-ns Ready
pass "case 1: gate 1 refused, then healed on resync after designation"

# detach + un-designate to restore a clean baseline
k delete remotenamespace rn-heal -n tenant-ns --timeout=60s
wait_gone serviceaccount argo-attach-tenant-ns team-c
k delete argoattachauthority tenant

log "applying baseline authorities (argocd -> *, argocd-two -> team-b)"
k apply -f "$FIXTURES/authorities.yml"

# --- case 2: gate 2 -------------------------------------------------------------

log "case 2: target outside allowedTargets is refused"
cat <<EOF | k apply -f -
apiVersion: field.vmware.com/v1
kind: RemoteNamespace
metadata: {name: rn-bounds, namespace: argocd-two}
spec: {targetNamespace: team-a, project: default}
EOF
wait_for_status remotenamespace rn-bounds argocd-two Failed "not allowed"
k delete remotenamespace rn-bounds -n argocd-two --timeout=60s
pass "case 2: allowedTargets bound enforced"

# --- case 3: gate 3 denylist floor ----------------------------------------------

log "case 3: denylist floor overrides the wildcard authority"
for target in kube-system vmware-system-test argocd-two blocked-ns-test; do
	name="rn-floor-${target}"
	cat <<EOF | k apply -f -
apiVersion: field.vmware.com/v1
kind: RemoteNamespace
metadata: {name: $name, namespace: argocd}
spec: {targetNamespace: $target, project: default}
EOF
	wait_for_status remotenamespace "$name" argocd Failed
	assert_absent serviceaccount argo-attach-argocd "$target"
	k delete remotenamespace "$name" -n argocd --timeout=60s
done
pass "case 3: kube-system, vmware-system-*, designated namespaces, and blocked-ns all refused"

# --- case 4: happy path ----------------------------------------------------------

log "case 4: happy path attach"
cat <<EOF | k apply -f -
apiVersion: field.vmware.com/v1
kind: RemoteNamespace
metadata: {name: rn-team-a, namespace: argocd}
spec:
  targetNamespace: team-a
  project: default
  clusterLabels: {env: e2e}
EOF
wait_for_status remotenamespace rn-team-a argocd Ready
assert_exists serviceaccount argo-attach-argocd team-a
assert_exists rolebinding argo-attach-argocd team-a
assert_jsonpath rolebinding argo-attach-argocd team-a '{.roleRef.name}' edit
assert_exists secret argo-attach-argocd-token team-a
CLUSTER_SECRET=supervisor-ns-team-a-argo-cluster
assert_exists secret "$CLUSTER_SECRET" argocd
assert_jsonpath secret "$CLUSTER_SECRET" argocd '{.metadata.labels.argocd\.argoproj\.io/secret-type}' cluster
assert_jsonpath secret "$CLUSTER_SECRET" argocd '{.metadata.labels.env}' e2e
[[ "$(k get secret "$CLUSTER_SECRET" -n argocd -o jsonpath='{.data.project}' | base64 -d)" == "default" ]] || fail "cluster secret project mismatch"
[[ "$(k get secret "$CLUSTER_SECRET" -n argocd -o jsonpath='{.data.namespaces}' | base64 -d)" == "team-a" ]] || fail "cluster secret namespaces mismatch"
[[ "$(k get secret "$CLUSTER_SECRET" -n argocd -o jsonpath='{.data.server}' | base64 -d)" == *"?context=team-a"* ]] || fail "cluster secret server mismatch"
# inventory recorded in status
NS_UID=$(k get namespace team-a -o jsonpath='{.metadata.uid}')
assert_jsonpath remotenamespace rn-team-a argocd '{.status.attachedNamespaceUID}' "$NS_UID"
assert_jsonpath remotenamespace rn-team-a argocd '{.status.serviceAccount}' argo-attach-argocd
# the minted token actually authenticates against the API server
ATTACH_TOKEN=$(k get secret argo-attach-argocd-token -n team-a -o jsonpath='{.data.token}' | base64 -d)
HTTP_CODE=$(curl -sk -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ATTACH_TOKEN" "$SERVER/api")
[[ "$HTTP_CODE" != "401" && "$HTTP_CODE" != "000" ]] || fail "attach token failed to authenticate (HTTP $HTTP_CODE)"
pass "case 4: SA/RoleBinding/token provisioned, cluster secret correct, inventory recorded, token authenticates"

# --- case 5: coexistence with a local ArgoNamespace ------------------------------

log "case 5: local ArgoNamespace coexists with the remote attach on the same target"
cat <<EOF | k apply -f -
apiVersion: field.vmware.com/v1
kind: ArgoNamespace
metadata: {name: local-team-a, namespace: team-a}
spec: {argoNamespace: argocd, project: default, serviceAccount: ""}
EOF
wait_for_status argonamespace local-team-a team-a Ready
assert_exists serviceaccount argo-attach-sa team-a
assert_exists serviceaccount argo-attach-argocd team-a
pass "case 5: distinct SA names, both attaches coexist"

# --- case 6: cleanup guard --------------------------------------------------------

log "case 6: deleting a never-provisioned CR cannot tear down a working attach"
cat <<EOF | k apply -f -
apiVersion: field.vmware.com/v1
kind: RemoteNamespace
metadata: {name: rn-sabotage, namespace: tenant-ns}
spec: {targetNamespace: team-a, project: default}
EOF
wait_for_status remotenamespace rn-sabotage tenant-ns Failed "not designated"
k delete remotenamespace rn-sabotage -n tenant-ns --timeout=60s
assert_exists serviceaccount argo-attach-argocd team-a
assert_exists secret argo-attach-argocd-token team-a
assert_exists rolebinding argo-attach-argocd team-a
assert_exists secret "$CLUSTER_SECRET" argocd
pass "case 6: working attach untouched by the failed CR's deletion"

# --- case 7: namespace recreation (UID pinning) -----------------------------------

log "case 7: recreated target namespace is not silently re-attached"
cat <<EOF | k apply -f -
apiVersion: field.vmware.com/v1
kind: RemoteNamespace
metadata: {name: rn-team-b, namespace: argocd-two}
spec: {targetNamespace: team-b, project: default}
EOF
wait_for_status remotenamespace rn-team-b argocd-two Ready
k delete namespace team-b --timeout=120s
k create namespace team-b
wait_for_status remotenamespace rn-team-b argocd-two Failed "recreated"
assert_absent serviceaccount argo-attach-argocd-two team-b
# deletion still completes despite the UID mismatch (no wedged finalizer)
k delete remotenamespace rn-team-b -n argocd-two --timeout=60s
pass "case 7: UID pinning blocked re-attach; CR still deletable"

# --- case 8: detach ----------------------------------------------------------------

log "case 8: detach removes only this attach's resources"
k delete remotenamespace rn-team-a -n argocd --timeout=60s
wait_gone serviceaccount argo-attach-argocd team-a
assert_absent secret argo-attach-argocd-token team-a
assert_absent rolebinding argo-attach-argocd team-a
# local attach survives; its resync recreates the shared cluster secret
assert_exists serviceaccount argo-attach-sa team-a
wait_exists secret "$CLUSTER_SECRET" argocd
k delete argonamespace local-team-a -n team-a --timeout=60s
wait_gone serviceaccount argo-attach-sa team-a
wait_gone secret "$CLUSTER_SECRET" argocd

log "case 8b: detach with the target namespace already gone"
cat <<EOF | k apply -f -
apiVersion: field.vmware.com/v1
kind: RemoteNamespace
metadata: {name: rn-team-c, namespace: argocd}
spec: {targetNamespace: team-c, project: default}
EOF
wait_for_status remotenamespace rn-team-c argocd Ready
k delete namespace team-c --timeout=120s
k delete remotenamespace rn-team-c -n argocd --timeout=60s
pass "case 8: detach clean in both variants, no wedged finalizers"

# --- case 9: ArgoCluster with a mock workload kubeconfig ---------------------------

log "case 9: ArgoCluster attach from a mock kubeconfig secret"
CLIENT_CERT=$(k config view --raw -o jsonpath='{.users[0].user.client-certificate-data}')
CLIENT_KEY=$(k config view --raw -o jsonpath='{.users[0].user.client-key-data}')
cat >"$WORKDIR/workload1.kubeconfig" <<EOF
apiVersion: v1
kind: Config
clusters:
- name: workload1
  cluster:
    server: $SERVER
    certificate-authority-data: $CADATA
users:
- name: workload1-admin
  user:
    client-certificate-data: $CLIENT_CERT
    client-key-data: $CLIENT_KEY
contexts:
- name: workload1-admin@workload1
  context: {cluster: workload1, user: workload1-admin}
current-context: workload1-admin@workload1
EOF
k create secret generic workload1-kubeconfig -n tenant-ns --from-file=value="$WORKDIR/workload1.kubeconfig"
cat <<EOF | k apply -f -
apiVersion: field.vmware.com/v1
kind: ArgoCluster
metadata: {name: workload1, namespace: tenant-ns}
spec: {clusterName: workload1, argoNamespace: argocd, project: default}
EOF
wait_for_status argocluster workload1 tenant-ns Ready
assert_exists secret workload1-argo-cluster argocd
[[ "$(k get secret workload1-argo-cluster -n argocd -o jsonpath='{.data.clusterresources}' | base64 -d)" == "true" ]] || fail "clusterresources not set"
[[ "$(k get secret workload1-argo-cluster -n argocd -o jsonpath='{.data.config}' | base64 -d)" == *"certData"* ]] || fail "TLS config missing certData"
k delete argocluster workload1 -n tenant-ns --timeout=60s
wait_gone secret workload1-argo-cluster argocd

log "case 9b: blocked argoNamespace refused on both legacy CRDs"
cat <<EOF | k apply -f -
apiVersion: field.vmware.com/v1
kind: ArgoCluster
metadata: {name: workload1-blocked, namespace: tenant-ns}
spec: {clusterName: workload1, argoNamespace: blocked-ns-test, project: default}
EOF
wait_for_status argocluster workload1-blocked tenant-ns Failed "blocked"
k delete argocluster workload1-blocked -n tenant-ns --timeout=60s
cat <<EOF | k apply -f -
apiVersion: field.vmware.com/v1
kind: ArgoNamespace
metadata: {name: local-blocked, namespace: tenant-ns}
spec: {argoNamespace: blocked-ns-test, project: default, serviceAccount: ""}
EOF
wait_for_status argonamespace local-blocked tenant-ns Failed "blocked"
k delete argonamespace local-blocked -n tenant-ns --timeout=60s
pass "case 9: ArgoCluster attach works; blocked-ns enforced on ArgoCluster AND ArgoNamespace"

log "ALL CASES PASSED"
