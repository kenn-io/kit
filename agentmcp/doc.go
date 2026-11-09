// Package agentmcp registers Model Context Protocol servers for agent clients.
// [MarshalConfig] renders configuration, and [Install] replaces one named entry.
// [Command] renders manual registration for Claude Code and Codex.
// [Check] compares the active user registration and asks the agent whether the
// server connects. Its result distinguishes missing or outdated configuration,
// connection failures, successful tool discovery, and inconclusive probes.
package agentmcp
