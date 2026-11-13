// Package agentsetup coordinates setup and readiness reports for agent integrations.
// Applications provide Items for hooks, MCP, and their own checked pieces.
// Installers run once in order and must be idempotent and independent. Combine
// dependent operations in one item.
package agentsetup
