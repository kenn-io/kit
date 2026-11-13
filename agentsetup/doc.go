// Package agentsetup coordinates setup and readiness reports for agent integrations.
// Applications provide Items for hooks, MCP, and their own checked pieces.
// Installers run once in order and must be idempotent and independent. Combine
// dependent operations in one item.
// Explicit agent names may include letters, digits, underscores, and hyphens,
// and cannot start with a hyphen. Unknown profiles have no built-in installer
// and report unknown readiness. Known profiles can install even when readiness
// inspection is unavailable.
//
// Claude Code and Codex MCP setup keeps user settings when the supplied entry
// contains only launch fields and env and uses the same transport. App env values
// override matching user values. Env keys the app stops supplying remain until
// manually removed. Args follow the application, so hand-added arguments make
// Check report outdated.
// Other entries are replaced.
//
// RepairCommand supplies the next step for missing or outdated built-in items.
// Repairable custom items retain their own next step alongside that command.
// When a next step is absent, RepairCommand supplies it, or SetupCommand if
// RepairCommand is empty.
package agentsetup
