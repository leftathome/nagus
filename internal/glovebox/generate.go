package glovebox

// openapi.yaml is glovebox's own contract (glovebox api/openapi.yaml at
// 2c1e6ac, 2026-07-03), vendored so the client is generated from the same
// file glovebox generates its server from and cannot drift from it by hand.
// Refresh it when glovebox's contract changes, then re-run go generate.

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen -config oapi-codegen.yaml openapi.yaml
