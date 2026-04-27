//go:build e2e

// Package e2e contains end-to-end tests that drive the live docker-compose
// stack. They are tagged //go:build e2e so the default `go test ./...` does
// not require a running stack.
package e2e
