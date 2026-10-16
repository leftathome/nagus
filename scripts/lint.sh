#!/bin/sh
# Run golangci-lint v2 over the whole module with the repo's .golangci.yml.
# This is the exact command the CI `lint` job runs (nagus-cxj), so a local
# pass means the job passes.
#
# Uses a golangci-lint already on PATH when it is the pinned version; otherwise
# falls back to `go run` of the pinned version (slow the first time, cached
# after). Keep GOLANGCI_LINT_VERSION in step with the image digest pinned in
# .gitlab-ci.yml (GOLANGCI_LINT_IMAGE).
#
# golangci-lint must be built with a Go at least as new as the one that loads
# the packages: a v2.12.x binary panics against a newer minor (e.g. go1.27).
# Use a Go 1.26.x toolchain locally.
#
# Extra arguments are passed through, e.g. scripts/lint.sh --fix
set -eu

GOLANGCI_LINT_VERSION="${GOLANGCI_LINT_VERSION:-v2.12.2}"

cd "$(dirname "$0")/.."

if command -v golangci-lint >/dev/null 2>&1 &&
	golangci-lint version 2>/dev/null | grep -q "version ${GOLANGCI_LINT_VERSION#v} "; then
	exec golangci-lint run "$@" ./...
fi
exec go run "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}" run "$@" ./...
