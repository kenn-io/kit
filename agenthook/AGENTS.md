# Agent Hook Package Invariants

- The exported event and tool vocabulary follows Claude Code. Agent-specific
  names belong in profiles; callers should not need native event names to
  describe equivalent hooks.
- Profiles own config discovery, native event and tool translation, file
  format, and format-specific validation. Add support for a new harness here
  rather than in each consuming application.
- JSON harnesses use either Claude-style nested handlers or native direct
  entries. Keep those encodings separate, and let each profile own event names,
  timeout units, timeout fields, and cross-platform command fields.
- Keep each harness profile in its own agent-named file (`claude.go`,
  `codex.go`, and so on). `profile.go` owns only the shared vocabulary,
  registry, and lookup behavior.
- A marker identifies application-owned commands. Install and uninstall may
  replace or remove only matching commands and must preserve unrelated hooks
  and top-level config.
- Do not silently enable an agent's hook auto-approval setting. Installation
  writes registrations; the harness remains responsible for user consent.
- JSON and YAML writes must preserve symlinked config paths and existing file
  permissions. Keep replacement behavior explicit on Unix and Windows.
