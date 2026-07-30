// Package daemon provides reusable infrastructure for local background
// daemons used by CLI tools.
//
// It covers the lifecycle pieces that should be shared across tools:
// endpoint parsing and HTTP clients for TCP or Unix sockets, atomic
// daemon.<pid>.json runtime files, PID plus ping-based discovery, and a small
// Manager that can auto-start an application daemon through a caller-provided
// StartFunc. Manager uses runtime-file discovery by default and accepts a
// FindFunc when callers need custom probing while retaining its start locking,
// re-discovery, polling, and timeout behavior.
//
// Authenticated discovery must prove an endpoint before any bearer credential
// is sent. Servers construct a Proof from the shared daemon token and register
// its NewPingHandler result using the same RuntimeRecord they write to
// RuntimeStore. Clients construct their own Proof from that token, place it in
// DiscoverOptions.Proof, and wait for Manager.Find or Manager.Ensure to succeed
// before using a credential-bearing client. The package sends a random
// challenge and verifies an identity-bound HMAC; it does not send or expose the
// token. Custom FindFunc callbacks must preserve this ordering and can use
// Proof.Probe directly.
//
// Application-specific server setup, database wiring, auth policy, CLI
// commands, and shutdown behavior remain in the owning tool.
package daemon
