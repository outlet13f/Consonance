#!/usr/bin/env bash
# Smoke test for consonance/postgres: the supervisor starts postgres, answers its API,
# and shuts postgres down cleanly when the container stops.
set -euo pipefail
IMAGE=${IMAGE:-consonance/postgres:17-dev}
NAME=consonance-smoke-$$
TOKEN=smoke-token
PORT=${PORT:-18008}

cleanup() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker volume rm "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Spec 7.3: the supervisor shuts postgres down on SIGTERM. The postgres base image stops with SIGINT.
stopsignal=$(docker image inspect --format '{{.Config.StopSignal}}' "$IMAGE")
[ "$stopsignal" = SIGTERM ] || { echo "FAIL: image stop signal is '$stopsignal', want SIGTERM"; exit 1; }

docker volume create "$NAME" >/dev/null
docker run --rm -v "$NAME:/pgdata" --user 0 --entrypoint chown "$IMAGE" 999:999 /pgdata
docker run --rm -v "$NAME:/pgdata" --entrypoint bash "$IMAGE" -c \
  'initdb -D /pgdata/data-1 -U postgres --auth-local=peer --auth-host=scram-sha-256 >/dev/null &&
   ln -s data-1 /pgdata/current && printf %s "$1" > /pgdata/token' _ "$TOKEN"

docker run -d --name "$NAME" -v "$NAME:/pgdata" -p "$PORT:8008" "$IMAGE" --token-file /pgdata/token >/dev/null

status=""
for _ in $(seq 1 150); do
  status=$(curl -fsS -H "X-Consonance-Token: $TOKEN" "localhost:$PORT/v1/status" 2>/dev/null || true)
  case "$status" in *'"pg":"running"'*) break ;; esac
  sleep 0.2
done
case "$status" in *'"pg":"running"'*) ;; *) docker logs "$NAME"; echo "FAIL: postgres not running"; exit 1 ;; esac

curl -fsS "localhost:$PORT/readyz" >/dev/null
code=$(curl -s -o /dev/null -w '%{http_code}' "localhost:$PORT/v1/status")
[ "$code" = 401 ] || { echo "FAIL: unauthenticated status returned $code"; exit 1; }

curl -fsS -X POST -H "X-Consonance-Token: $TOKEN" -d '{"label":"smoke"}' "localhost:$PORT/v1/backup/start" >/dev/null
curl -fsS -X POST -H "X-Consonance-Token: $TOKEN" "localhost:$PORT/v1/backup/stop" | grep -q 'LABEL: smoke'

docker stop -t 40 "$NAME" >/dev/null
docker logs "$NAME" 2>&1 | grep -q 'database system is shut down' || { docker logs "$NAME"; echo "FAIL: no clean shutdown"; exit 1; }
echo PASS
