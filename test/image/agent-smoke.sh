#!/usr/bin/env bash
# Smoke test for consonance/agent: the agent serves its API on a volume, refuses requests
# without the token, makes main's data with initdb, and exits cleanly on SIGTERM.
set -euo pipefail
IMAGE=${IMAGE:-consonance/agent:dev}
NAME=consonance-agent-smoke-$$
TOKEN=smoke-token
PORT=${PORT:-18443}

cleanup() {
  docker rm -f -v "$NAME" >/dev/null 2>&1 || true
  docker volume rm "$NAME" "$NAME-token" >/dev/null 2>&1 || true
}
trap cleanup EXIT

stopsignal=$(docker image inspect --format '{{.Config.StopSignal}}' "$IMAGE")
[ "$stopsignal" = SIGTERM ] || { echo "FAIL: image stop signal is '$stopsignal', want SIGTERM"; exit 1; }
user=$(docker image inspect --format '{{.Config.User}}' "$IMAGE")
[ "$user" = 999:999 ] || { echo "FAIL: image user is '$user', want 999:999"; exit 1; }
entrypoint=$(docker image inspect --format '{{json .Config.Entrypoint}}' "$IMAGE")
[ "$entrypoint" = '["/usr/bin/tini","--","/usr/local/bin/consonance-agent"]' ] ||
  { echo "FAIL: image entrypoint is $entrypoint, want tini then the agent"; exit 1; }

# The project volume: a new volume takes the owner of the image's /volume (999).
docker volume create "$NAME" >/dev/null
docker volume create "$NAME-token" >/dev/null
docker run --rm --user 0 -v "$NAME-token:/token" --entrypoint sh "$IMAGE" \
  -c 'printf %s "$1" > /token/token && chmod 644 /token/token' _ "$TOKEN"

# Spec 7.5: the agent pod runs PSS restricted with 128Mi; initdb must fit.
docker run -d --name "$NAME" -v "$NAME:/volume" -v "$NAME-token:/etc/consonance/token:ro" \
  --cap-drop ALL --security-opt no-new-privileges --memory 128m --memory-swap 128m \
  -p "127.0.0.1:$PORT:8443" "$IMAGE" >/dev/null

for _ in $(seq 1 100); do
  curl --max-time 2 -fsS "localhost:$PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
curl --max-time 2 -fsS "localhost:$PORT/healthz" >/dev/null || { docker logs "$NAME"; echo "FAIL: the agent does not answer"; exit 1; }

code=$(curl --max-time 10 -s -o /dev/null -w '%{http_code}' "localhost:$PORT/v1/usage")
[ "$code" = 401 ] || { echo "FAIL: usage without the token returned $code"; exit 1; }

auth=(-H "X-Consonance-Token: $TOKEN")
# Preflight answers on any filesystem: reflink is true on XFS or btrfs, false elsewhere.
preflight=$(curl --max-time 10 -fsS "${auth[@]}" "localhost:$PORT/v1/preflight")
case "$preflight" in *'"reflink":true'* | *'"reflink":false'*) ;; *) echo "FAIL: preflight: $preflight"; exit 1 ;; esac

init=$(curl --max-time 60 -fsS -X POST "${auth[@]}" "localhost:$PORT/v1/init-main")
case "$init" in *'"generation":1'*) ;; *) docker logs "$NAME"; echo "FAIL: init-main: $init"; exit 1 ;; esac
gens=$(curl --max-time 10 -fsS "${auth[@]}" "localhost:$PORT/v1/branches/main/generations")
case "$gens" in *'"current":1'*) ;; *) echo "FAIL: generations: $gens"; exit 1 ;; esac

docker stop -t 30 "$NAME" >/dev/null
exit_code=$(docker inspect --format '{{.State.ExitCode}}' "$NAME")
[ "$exit_code" = 0 ] || { docker logs "$NAME"; echo "FAIL: the agent exited with $exit_code"; exit 1; }
echo PASS
