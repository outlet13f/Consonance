#!/usr/bin/env bash
# Installs Consonance into the dev cluster: the admin key Secret, then config/default (CRDs,
# RBAC, admission policy, operator, API server). Run it after `make dev-cluster images load-images`.
#
# The admin key is bin/dev/admin-key (git-ignored, 32 random bytes as base64url, 43 characters).
# It is made once and never replaced, so the key a client holds stays valid across re-runs; the
# script never prints it (it fixes the file's mode to 600 on every run). Delete the file to get a new key (and rerun to roll it out; the API
# server reads the key at start, so `kubectl -n consonance-system rollout restart
# deployment/consonance-apiserver` follows).
#
# It uses bin/dev/kubeconfig (or $DEV_KUBECONFIG), not $KUBECONFIG.
#
# Usage: hack/deploy-dev.sh up | down
#   up    (default) idempotent install
#   down  remove the operator and the API server. CRDs, RBAC, the namespace and the key stay:
#         deleting the CRDs would delete every project with them.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
KEY_FILE=$ROOT/bin/dev/admin-key
NS=consonance-system
# Always the dev cluster's kubeconfig, never the ambient $KUBECONFIG: this script must not touch
# whatever cluster the shell happens to point at. DEV_KUBECONFIG overrides it on purpose.
export KUBECONFIG=${DEV_KUBECONFIG:-$ROOT/bin/dev/kubeconfig}

[ -f "$KUBECONFIG" ] || { echo "no kubeconfig at $KUBECONFIG; run 'make dev-cluster' first" >&2; exit 1; }
echo "cluster: $(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"

up() {
  if [ ! -s "$KEY_FILE" ]; then
    mkdir -p "$(dirname "$KEY_FILE")"
    (
      umask 077
      head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n' >"$KEY_FILE.tmp"
      mv "$KEY_FILE.tmp" "$KEY_FILE"
    )
    echo "made the admin key in ${KEY_FILE#"$ROOT"/}"
  fi
  chmod 600 "$KEY_FILE"
  [ "$(wc -c <"$KEY_FILE")" -ge 32 ] || { echo "${KEY_FILE#"$ROOT"/} is shorter than 32 bytes; delete it and rerun" >&2; exit 1; }

  # The namespace first: the Secret goes in it before the Deployments mount it.
  kubectl apply -f "$ROOT/config/default/namespace.yaml"
  # Server-side apply: a client-side apply stores the whole object, key included, in the
  # last-applied-configuration annotation. Remove that annotation if an earlier run left it.
  kubectl -n "$NS" create secret generic consonance-admin-key --from-file=key="$KEY_FILE" \
    --dry-run=client -o yaml | kubectl apply --server-side --force-conflicts -f - >/dev/null
  kubectl -n "$NS" annotate secret consonance-admin-key kubectl.kubernetes.io/last-applied-configuration- >/dev/null
  echo "secret/consonance-admin-key applied"

  kubectl apply -k "$ROOT/config/default"
  kubectl -n "$NS" rollout status deployment/consonance-operator --timeout=180s
  kubectl -n "$NS" rollout status deployment/consonance-apiserver --timeout=180s
  kubectl -n "$NS" wait --for=condition=Available deployment --all --timeout=60s
}

down() {
  kubectl -n "$NS" delete deployment/consonance-operator deployment/consonance-apiserver \
    service/consonance-apiserver --ignore-not-found
}

case "${1:-up}" in
  up) up ;;
  down) down ;;
  *) echo "usage: $0 up | down" >&2; exit 2 ;;
esac
