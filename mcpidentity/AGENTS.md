# MCP identity invariants

- Apps supply all metadata and image bytes. Never add product branding,
  network fetching, filesystem reads or credential access.
- Keep SDK metadata identical across stdio and HTTP. Let the SDK serialize
  initialization and modern per-result serverInfo instead of patching replies.
- Copy input bytes and returned mutable metadata. Shared identity is public
  application metadata, never an authenticated request principal.
- Public image handlers serve only their two exact paths and GET/HEAD. Mount
  them separately from authenticated MCP; do not weaken endpoint authentication.
