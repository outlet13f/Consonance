FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/consonance-supervisor ./cmd/supervisor

FROM postgres:17-bookworm
RUN apt-get update \
 && apt-get install -y --no-install-recommends tini \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/consonance-supervisor /usr/local/bin/consonance-supervisor
USER 999:999
# tini is PID 1: it reaps orphaned processes and forwards SIGTERM to the supervisor.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/consonance-supervisor"]
