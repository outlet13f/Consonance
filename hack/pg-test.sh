#!/usr/bin/env bash
# Runs `go test` in a container on the Podman machine, which has PostgreSQL 17 and an XFS
# filesystem with reflink (the machine's /var), so the tests that need them do not skip.
# Usage: hack/pg-test.sh [go test args...]   (default: ./...)
set -euo pipefail
IMAGE=${IMAGE:-consonance/test:dev}
XFS_HOST_DIR=/var/lib/consonance/test   # on the machine, not the Mac
ROOT=$(cd "$(dirname "$0")/.." && pwd)
ARGS=("$@")
if [ ${#ARGS[@]} -eq 0 ]; then ARGS=(./...); fi

# The machine user owns the XFS directory; --userns=keep-id makes the container run as it.
podman machine ssh 'sudo install -d -o $(id -u) /var/lib/consonance/test'

if [ -n "${REBUILD:-}" ] || ! podman image exists "$IMAGE"; then
  podman build -f "$ROOT/images/test/Dockerfile" -t "$IMAGE" "$ROOT/images/test"
fi

TTY=()
if [ -t 0 ] && [ -t 1 ]; then TTY=(-t); fi

# The repository is mounted at its Mac path: the Podman machine sees /Users through virtiofs
# at the same path. virtiofs cannot be relabeled, so SELinux labeling is off for this container
# only (the machine's SELinux mode stays Enforcing). /tmp is a tmpfs because the container's
# own filesystem sits on the machine's XFS and would clone files: tests that use t.TempDir()
# as "not XFS" need a filesystem without reflink.
exec podman run --rm ${TTY[@]+"${TTY[@]}"} --userns=keep-id --security-opt label=disable \
  -v "$ROOT:$ROOT" -w "$ROOT" \
  -v consonance-gocache:/gocache \
  -v "$XFS_HOST_DIR:/xfs/test" \
  --tmpfs /tmp:rw,mode=1777 \
  -e HOME=/tmp \
  "$IMAGE" go test "${ARGS[@]}"
