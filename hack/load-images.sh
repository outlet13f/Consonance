#!/usr/bin/env bash
# Imports the four Consonance images into the containerd of the dev cluster (consonance-k3s on
# the Podman machine). The images are built into the machine's rootless engine by `make images`;
# this streams each one with `docker save` into `ctr images import` in the rootful k3s container,
# without temp files. Safe to repeat: importing replaces an image of the same name.
#
# Manifests refer to the images as consonance/<name>:<tag> with imagePullPolicy: Never; containerd
# stores them under the normalized name docker.io/consonance/<name>:<tag>, which is the same image.
#
# Usage: hack/load-images.sh
set -euo pipefail

NAME=consonance-k3s
IMAGES=(consonance/postgres:17-dev consonance/agent:dev consonance/operator:dev consonance/apiserver:dev)

state=$(podman machine ssh sudo podman inspect -f '{{.State.Status}}' "$NAME" 2>/dev/null || true)
[ "$state" = running ] || { echo "$NAME is not running; run 'make dev-cluster' first" >&2; exit 1; }

for image in "${IMAGES[@]}"; do
  docker image inspect "$image" >/dev/null 2>&1 || { echo "$image is not built; run 'make images' first" >&2; exit 1; }
done

for image in "${IMAGES[@]}"; do
  echo "loading $image"
  docker save "$image" | podman machine ssh sudo podman exec -i "$NAME" ctr -n k8s.io images import -
done

podman machine ssh sudo podman exec "$NAME" crictl images | grep -E '^IMAGE|consonance/'
