#!/usr/bin/env bash
# Development Kubernetes cluster: k3s in a rootful privileged container `consonance-k3s` on the
# Podman machine, with the StorageClass `consonance-xfs` backed by the machine's XFS (reflink).
#
# HOW TO RUN kubectl AGAINST IT (every later task uses exactly this):
#   export KUBECONFIG=$PWD/bin/dev/kubeconfig     # written by `make dev-cluster`
#   kubectl get nodes
# The Podman machine publishes the rootful port 6443 to the Mac (gvproxy), so the kubeconfig
# points at https://127.0.0.1:6443 and the Mac's own kubectl works. If 6443 is ever not
# reachable from the Mac, `kubeconfig` instead writes the wrapper bin/dev/kubectl
# (podman machine ssh sudo podman exec -i consonance-k3s kubectl "$@") and the way is
#   bin/dev/kubectl get nodes
#
# Usage: hack/dev-cluster.sh up | down [--purge] | kubeconfig | check
#   up          create (or start) the cluster, wait for the node, apply the StorageClass,
#               write the kubeconfig. Safe to repeat: a running cluster is left alone.
#   down        remove the container. Volumes in /var/lib/consonance/storage stay unless --purge.
#   kubeconfig  rewrite bin/dev/kubeconfig (and the wrapper if 6443 is not reachable).
#   check       prove that consonance-xfs volumes are on XFS and support reflink, in a
#               throwaway restricted pod; everything it creates is deleted afterwards.
#
# Container flags that k3s needs here: --privileged, --cgroupns=host, --tmpfs /run and
# /var/run, and --security-opt label=disable (the machine's SELinux stays Enforcing).
set -euo pipefail

NAME=consonance-k3s
IMAGE=${K3S_IMAGE:-docker.io/rancher/k3s:v1.36.5-k3s1}
STORAGE_DIR=/var/lib/consonance/storage   # on the machine, not the Mac
PORT=6443
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=$ROOT/bin/dev

# Runs a command in the machine's rootful engine; stdin passes through.
rpodman() { podman machine ssh sudo podman "$@"; }
kc() { rpodman exec -i "$NAME" kubectl "$@"; }

state() { rpodman inspect -f '{{.State.Status}}' "$NAME" 2>/dev/null || true; }

up() {
  podman machine ssh "sudo install -d $STORAGE_DIR"
  case "$(state)" in
    running) echo "$NAME is already running" ;;
    exited | created | stopped) rpodman start "$NAME" >/dev/null ;;
    "")
      rpodman run -d --name "$NAME" --privileged --security-opt label=disable \
        --cgroupns=host --tmpfs /run --tmpfs /var/run \
        -v /var/lib/consonance:/var/lib/consonance -p "$PORT:6443" \
        "$IMAGE" server --disable traefik \
        --default-local-storage-path "$STORAGE_DIR" --write-kubeconfig-mode 644 >/dev/null
      ;;
    *) echo "$NAME is in state $(state); remove it with 'down' first" >&2; exit 1 ;;
  esac

  echo "waiting for the node to be Ready"
  local i
  for i in $(seq 1 90); do
    if kc wait --for=condition=Ready node --all --timeout=5s >/dev/null 2>&1; then break; fi
    if [ "$i" = 90 ]; then echo "node did not become Ready" >&2; exit 1; fi
    sleep 2
  done

  kc apply -f - <"$ROOT/config/dev/storageclass.yaml"
  write_kubeconfig
}

write_kubeconfig() {
  mkdir -p "$OUT"
  # k3s writes the file with server https://127.0.0.1:6443, which is what the Mac reaches.
  rpodman exec "$NAME" cat /etc/rancher/k3s/k3s.yaml >"$OUT/kubeconfig.tmp"
  chmod 600 "$OUT/kubeconfig.tmp"
  mv "$OUT/kubeconfig.tmp" "$OUT/kubeconfig"
  if curl -sk -m 5 -o /dev/null "https://127.0.0.1:$PORT/version"; then
    rm -f "$OUT/kubectl"
    echo "kubeconfig: export KUBECONFIG=$OUT/kubeconfig"
  else
    cat >"$OUT/kubectl" <<WRAP
#!/usr/bin/env bash
exec podman machine ssh sudo podman exec -i $NAME kubectl "\$@"
WRAP
    chmod +x "$OUT/kubectl"
    echo "port $PORT is not reachable from this Mac; use $OUT/kubectl instead of kubectl"
  fi
}

down() {
  rpodman rm -f "$NAME" >/dev/null 2>&1 || true
  if [ "${1:-}" = "--purge" ]; then
    podman machine ssh "sudo rm -rf $STORAGE_DIR && sudo install -d $STORAGE_DIR"
    echo "purged $STORAGE_DIR"
  fi
  rm -f "$OUT/kubeconfig" "$OUT/kubectl"
}

check() {
  local ns=consonance-dev-check
  trap 'kc delete namespace '"$ns"' --ignore-not-found --wait=false >/dev/null 2>&1 || true' EXIT
  # The namespace enforces Pod Security `restricted`, so the pod below must satisfy it.
  kc apply -f - <<YAML
apiVersion: v1
kind: Namespace
metadata:
  name: $ns
  labels:
    pod-security.kubernetes.io/enforce: restricted
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: reflink
  namespace: $ns
spec:
  storageClassName: consonance-xfs
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 64Mi
---
apiVersion: v1
kind: Pod
metadata:
  name: reflink
  namespace: $ns
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: check
      image: docker.io/library/debian:bookworm-slim
      command:
        - sh
        - -ec
        - |
          echo "filesystem type: \$(stat -f -c %T /data)"
          head -c 1048576 /dev/urandom >/data/a
          cp --reflink=always /data/a /data/b
          cmp /data/a /data/b
          echo "reflink copy: ok"
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: reflink
YAML
  local phase="" i
  for i in $(seq 1 90); do
    phase=$(kc get pod reflink -n "$ns" -o jsonpath='{.status.phase}')
    case "$phase" in Succeeded | Failed) break ;; esac
    sleep 2
  done
  kc logs reflink -n "$ns" || true
  local pv
  pv=$(kc get pvc reflink -n "$ns" -o jsonpath='{.spec.volumeName}')
  echo "volume directory on the machine: $(podman machine ssh "sudo ls -d $STORAGE_DIR/${pv}_${ns}_reflink" 2>&1)"
  # The pod and the PVC go first so the provisioner removes the volume directory.
  kc delete pod reflink -n "$ns" --wait=true >/dev/null
  kc delete pvc reflink -n "$ns" --wait=true >/dev/null
  kc delete namespace "$ns" --wait=true >/dev/null
  trap - EXIT
  [ "$phase" = Succeeded ] || { echo "reflink check failed (pod phase: $phase)" >&2; exit 1; }
}

case "${1:-}" in
  up) up ;;
  down) down "${2:-}" ;;
  kubeconfig) write_kubeconfig ;;
  check) check ;;
  *) sed -n '2,/^set -euo/p' "$0" | sed '$d' >&2; exit 2 ;;
esac
