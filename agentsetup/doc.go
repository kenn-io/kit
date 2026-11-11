// Package agentsetup coordinates setup and readiness reports for agent integrations.
// Applications provide Items for hooks, MCP, and their own checked pieces.
// Installers run once in order and must be idempotent and independent. Combine
// dependent operations in one item.
// Checks run after all installs. Run requires SetupCommand as guidance for
// items with no next step; NewCommand derives it from the command path.
// Run with CheckOnly lets a doctor reuse the same report.
package agentsetup
