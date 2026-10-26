#!/usr/bin/env bash
# Smoke test for consonance/operator and consonance/apiserver: each image runs --help and exits 0,
# runs as 65532:65532 and has the binary as its entrypoint.
set -euo pipefail
for name in operator apiserver; do
  image=consonance/$name:dev
  user=$(docker image inspect --format '{{.Config.User}}' "$image")
  [ "$user" = 65532:65532 ] || { echo "FAIL: $image user is '$user', want 65532:65532"; exit 1; }
  entrypoint=$(docker image inspect --format '{{json .Config.Entrypoint}}' "$image")
  [ "$entrypoint" = "[\"/consonance-$name\"]" ] || { echo "FAIL: $image entrypoint is $entrypoint"; exit 1; }
  docker run --rm "$image" --help >/dev/null 2>&1 || { echo "FAIL: $image --help did not exit 0"; exit 1; }
  echo "ok: $image"
done
echo PASS
