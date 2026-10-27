# Environment for the unit tests that need real PostgreSQL 17 and XFS with reflink
# (CONSONANCE_TEST_PG_BIN, CONSONANCE_TEST_XFS_DIR); run by hack/pg-test.sh.
FROM golang:1.27-bookworm
# gcc is for cgo (go test -race). The cluster the package creates is dropped: the tests run
# initdb themselves, as the caller's uid (initdb refuses to run as root). The drop is allowed to
# fail; the install before it is not.
RUN export DEBIAN_FRONTEND=noninteractive \
 && apt-get update \
 && apt-get install -y --no-install-recommends postgresql-common ca-certificates \
 && /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y \
 && apt-get install -y --no-install-recommends postgresql-17 xfsprogs gcc libc6-dev \
 && rm -rf /var/lib/apt/lists/* \
 && (pg_dropcluster --stop 17 main 2>/dev/null || true)
# The Go caches live in a named volume. A volume copies the owner and mode of its mount point
# from the image, so the non-root user that hack/pg-test.sh runs as can write to it.
RUN install -d -m 0777 /gocache
ENV GOCACHE=/gocache/build GOMODCACHE=/gocache/mod GOFLAGS=-buildvcs=false \
    CONSONANCE_TEST_PG_BIN=/usr/lib/postgresql/17/bin CONSONANCE_TEST_XFS_DIR=/xfs/test
