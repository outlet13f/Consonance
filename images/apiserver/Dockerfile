FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/consonance-apiserver ./cmd/apiserver

# One static binary on distroless: no shell, no package manager, uid 65532 (Pod Security restricted).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/consonance-apiserver /consonance-apiserver
USER 65532:65532
ENTRYPOINT ["/consonance-apiserver"]
