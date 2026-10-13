#!/usr/bin/env bash
# One-time setup of the Lima VM used for tests that need PostgreSQL 17 or XFS with reflink.
set -euo pipefail
VM=${VM:-consonance-poc}
GO_VERSION=${GO_VERSION:-1.27.2}

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
  if ! command -v mkfs.xfs >/dev/null; then
    apt-get update -qq
    apt-get install -y -qq xfsprogs >/dev/null
  fi
  # XFS with reflink for internal/volume tests (CONSONANCE_TEST_XFS_DIR, spec 12). A loop file
  # stands in for a CSI volume; the fstab entry mounts it again after a VM restart.
  if [ ! -f /var/lib/consonance/xfs.img ]; then
    mkdir -p /var/lib/consonance
    truncate -s 8G /var/lib/consonance/xfs.img
    mkfs.xfs -q -m reflink=1 /var/lib/consonance/xfs.img
  fi
  mkdir -p /mnt/consonance-xfs
  grep -q ' /mnt/consonance-xfs ' /etc/fstab ||
    echo '/var/lib/consonance/xfs.img /mnt/consonance-xfs xfs loop,nofail 0 0' >> /etc/fstab
  mountpoint -q /mnt/consonance-xfs || mount /mnt/consonance-xfs
  install -d -o \$SUDO_UID -g \$SUDO_GID /mnt/consonance-xfs/test
  xfs_info /mnt/consonance-xfs | grep -q 'reflink=1'
  /usr/local/go/bin/go version
  /usr/lib/postgresql/17/bin/postgres --version
  echo 'xfs: /mnt/consonance-xfs (reflink=1); tests use /mnt/consonance-xfs/test'
"
