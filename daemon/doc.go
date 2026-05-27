// Package daemon provides reusable infrastructure for local background
// daemons used by CLI tools.
//
// It covers the lifecycle pieces that should be shared across tools:
// endpoint parsing and HTTP clients for TCP or Unix sockets, atomic
// daemon.<pid>.json runtime files, PID plus ping-based discovery, and a small
// Manager that can auto-start an application daemon through a caller-provided
// StartFunc.
//
// Application-specific server setup, database wiring, auth policy, CLI
// commands, and shutdown behavior remain in the owning tool.
package daemon
