FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/consonance-agent ./cmd/agent

# postgres:17 brings initdb (spec 7.6) and the postgres user, uid/gid 999.
FROM postgres:17-bookworm
RUN apt-get update \
 && apt-get install -y --no-install-recommends tini \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /volume \
 && chown 999:999 /volume
COPY --from=build /out/consonance-agent /usr/local/bin/consonance-agent
USER 999:999
STOPSIGNAL SIGTERM
# tini is PID 1: it reaps initdb's children and forwards SIGTERM to the agent.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/consonance-agent"]
