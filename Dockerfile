# Per-architecture base pins, ONE STAGE PER ARCH, selected by BUILD_ARCH below.
#
# Why per-arch digests rather than the multi-arch index digest: the in-cluster
# zot mirror on-demand-syncs whatever kaniko requests, and the full golang:1.26
# INDEX is ~3GB across 9 platforms, which the residential uplink cannot pull
# reliably (nagus-c4p). Referencing a SINGLE-PLATFORM manifest by digest makes
# zot/kaniko fetch only that platform (~312MB), which the link handles. Pinning
# the index here would re-break exactly what c4p fixed, so multi-arch is done by
# pinning both platforms separately and letting each native build leg pick its
# own -- each leg still pulls only its own ~312MB.
#
# Refresh these when intentionally bumping the base:
#   crane digest --platform linux/amd64 docker.io/library/golang:1.26
#   crane digest --platform linux/arm64 docker.io/library/golang:1.26
# TARGETARCH is the standard automatic platform ARG that the builder populates
# (kaniko does so from --custom-platform, which the ci-templates legs pass as
# linux/amd64 or linux/arm64; docker/buildkit set it from the target platform).
# It MUST be declared before the first FROM to be usable in a FROM instruction.
#
# Deliberately NOT defaulted. If a builder ever fails to populate it, the FROM
# below resolves to "build-" and the build fails LOUDLY. Defaulting it to amd64
# would instead make the arm64 leg silently build an amd64 binary and publish it
# under an arm64 platform declaration -- an index that passes a platform check
# while being wrong, which is far worse than a red pipeline.
ARG TARGETARCH
FROM docker.io/golang:1.26@sha256:dbb10bd1b3400ba0858e2f7c354fd4556b782c187feeff52789d4ee156a84db8 AS build-amd64
FROM docker.io/golang:1.26@sha256:d86488d9077169d6dd4fa32e954e8b68a41e94e32c0ec3d3fefdcc017ac9a759 AS build-arm64

FROM build-${TARGETARCH} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# CGO_ENABLED=0 keeps the binary static (pure-Go sqlite), so it drops into
# distroless/static. GOARCH is deliberately NOT set: each leg builds natively,
# so the toolchain's own default is already the target -- setting it would be a
# second source of truth that could disagree with the base image.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=${VERSION}" -o /nagus ./cmd/nagus

# distroless/static-debian12 is a multi-arch index and resolves to the building
# platform on its own, so it needs no per-arch handling.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /nagus /nagus
USER nonroot:nonroot
ENTRYPOINT ["/nagus"]
