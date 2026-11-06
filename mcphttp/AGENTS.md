# MCP HTTP invariants

- Keep this package separate from `agentmcp`, which owns client configuration.
- The caller supplies authentication and a fresh SDK server per request. Never
  retain identity, tool policy, backend state or credentials across requests.
- Preserve stateless JSON transport, body limits, SDK localhost protection,
  cross-origin checks and cancellation from both HTTP and the SDK.
- Own endpoint admission and draining only. Listener binding, TLS, proxy trust,
  resource cleanup and stdio belong to the application.
