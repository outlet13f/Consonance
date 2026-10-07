#!/usr/bin/env bash
# Runs `go test` inside the Lima VM where PostgreSQL 17 is installed.
# Usage: hack/vm-test.sh [go test args...]   (default: ./...)
set -euo pipefail
VM=${VM:-consonance-poc}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
ARGS=("$@")
if [ ${#ARGS[@]} -eq 0 ]; then ARGS=(./...); fi
QUOTED=$(printf ' %q' "${ARGS[@]}")
limactl shell "$VM" -- bash -c "cd $(printf %q "$ROOT") && export PATH=/usr/local/go/bin:\$PATH GOFLAGS=-buildvcs=false CONSONANCE_TEST_PG_BIN=/usr/lib/postgresql/17/bin && go test$QUOTED"
