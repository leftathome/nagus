# Pin the linux/amd64 manifest digest of golang:1.26 (not the multi-arch index).
# The in-cluster zot mirror on-demand-syncs whatever kaniko requests; the full
# golang:1.26 INDEX is ~3GB across 9 platforms, which the residential uplink
# cannot pull reliably (nagus-c4p). Referencing the amd64 manifest by digest
# makes zot/kaniko fetch ONLY linux/amd64 (~312MB), which the link handles.
# Refresh this digest when intentionally bumping the base:
#   crane digest --platform linux/amd64 docker.io/library/golang:1.26
FROM docker.io/golang:1.26@sha256:dbb10bd1b3400ba0858e2f7c354fd4556b782c187feeff52789d4ee156a84db8 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=${VERSION}" -o /nagus ./cmd/nagus

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /nagus /nagus
USER nonroot:nonroot
ENTRYPOINT ["/nagus"]
