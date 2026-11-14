# MCP skills invariants

- Only serve the ordered authored guides supplied to `New`. Never read runtime
  skill directories, accept paths, or execute guides.
- Keep tool names and guide content application-owned. `agentmcp` handles client
  configuration and is a separate concern.
- Preserve strict raw-handler input validation and independent result ownership
  for callers using `Tool` and `Handler` through a central catalog.
- Discovery, authentication, authorization, loading and transport belong to the
  server's application; keep this helper independent of those policies.
