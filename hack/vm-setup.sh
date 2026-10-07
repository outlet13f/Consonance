#!/usr/bin/env bash
# One-time setup of the Lima VM used for tests that need PostgreSQL 17 (and XFS in plan 2).
set -euo pipefail
VM=${VM:-consonance-poc}
GO_VERSION=${GO_VERSION:-1.25.6}

limactl start "$VM" >/dev/null 2>&1 || true
limactl shell "$VM" -- sudo bash -euo pipefail -c "
  export DEBIAN_FRONTEND=noninteractive
  ARCH=\$(dpkg --print-architecture)
  if ! /usr/local/go/bin/go version 2>/dev/null | grep -q 'go$GO_VERSION '; then
    curl -fsSL https://go.dev/dl/go$GO_VERSION.linux-\$ARCH.tar.gz -o /tmp/go.tgz
    rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz
  fi
  if ! command -v gcc >/dev/null; then
    apt-get update -qq
    apt-get install -y -qq gcc >/dev/null   # go test -race needs cgo
  fi
  if [ ! -x /usr/lib/postgresql/17/bin/postgres ]; then
    apt-get update -qq
    apt-get install -y -qq postgresql-common >/dev/null
    /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y >/dev/null
    apt-get install -y -qq postgresql-17 >/dev/null
    systemctl disable --now postgresql >/dev/null 2>&1 || true
  fi
  /usr/local/go/bin/go version
  /usr/lib/postgresql/17/bin/postgres --version
"
