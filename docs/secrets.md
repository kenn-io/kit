---
title: Secret sources
description: Configure literal, environment, and private-file secrets with secretref.
last_edited: 2026-11-09
---

# Secret sources

Use `secretref.Ref` for a new secret setting. A string always means the literal
secret, including strings that look like paths, environment references, or URIs.

```toml
token = "literal-value"
# Or choose one source:
# token = { value = "literal-value" }
# token = { env = "APP_TOKEN" }
# token = { file = "~/.config/example/token" }
```

The same string/object forms work in JSON. Set only one nonempty source. An
absent key, empty string, empty table, or `{ value = "" }` is unset. JSON `null`
is also unset. Nonempty whitespace-only secrets and secrets containing NUL fail
when resolved. Literal and environment values otherwise retain their bytes.
JSON object keys are exactly `value`, `env`, and `file`, with string values;
null object fields and case aliases are rejected. Repeated identical JSON keys
retain the standard decoder's last-value behavior; avoid duplicate keys in
configuration. The top-level null exception remains supported.

## Load, then resolve

Decode into `Ref`, call `Validate` during config loading, then resolve when the
secret is needed:

```go
if err := cfg.Token.Validate(); err != nil {
    return err
}
secret, err := cfg.Token.ResolveContext(ctx)
if err != nil {
    return err
}
client.SetToken(secret.Value)
```

Validation performs no source I/O. It rejects conflicting sources, invalid
environment selectors, and file selectors that are neither absolute nor start
with `~/`. It does not check whether a configured source exists. A configured
source that is missing, unreadable, blank, or invalid returns an error; there is
no fallback to another source. Callers decide whether an unset reference is
allowed. `Resolve` remains a compatibility wrapper using a background context;
new code should use `ResolveContext`. Cancellation is checked between filesystem
operations and reads; it cannot interrupt every kernel call.

TOML decoders that invoke `UnmarshalTOML` reject unknown source fields. A decoder
that fills exported struct fields can silently discard unknown keys before
`Validate` sees them. Enable that decoder's strict mode or reject its undecoded
keys at the configuration boundary. `Validate` cannot recover discarded keys.
`UnmarshalText` preserves the literal; validate after a field-based decode.

## Existing sibling keys and environment settings

Convert existing `token`, `token_file`, and `token_env` settings without inventing
precedence:

```go
ref, err := secretref.FromFields("token", secretref.Fields{
    Value: cfg.Token, File: cfg.TokenFile, Env: cfg.TokenEnv,
})
```

`FromFields` rejects multiple nonempty fields and names the conflicting keys.
New configuration formats should use one `Ref` key instead.

For process environment settings, `secretref.FromEnv("APP_TOKEN")` selects
`APP_TOKEN` or `APP_TOKEN_FILE`. Presence matters: both present is a conflict even
if one is empty; neither present yields an unset reference. Empty `APP_TOKEN`
fails at resolution and empty `APP_TOKEN_FILE` fails at selection. No file is
read during selection. Names must have no surrounding whitespace, `=`, or NUL.
Direct `Ref{Env: name}` retains its existing trim-on-resolution behavior;
`FromEnv` rejects edge-spaced names so selection and resolution agree.

## File policy

`safefileio.OpenSecretFile(ctx, path)` opens and validates the same regular-file
handle returned to its caller. It never repairs permissions. Unix rules:

- The final file belongs to the effective user with mode `0400` or `0600`, or
  belongs to root without group/other write permission. Root-owned mounted
  credentials can therefore be readable without being user-owned.
- Every traversed directory belongs to root or the effective user and denies
  group/other write. A trusted sticky directory is allowed when its child also
  belongs to root or the effective user.
- Symlinks are allowed only through trusted parents and root/user-owned link
  entries. Every target path is checked too. This supports projected credential
  layouts such as Kubernetes `token -> ..data/token`. Loops and more than 40
  symlink hops fail. Components before `..` are checked before traversal.
- Linux file access ACLs and unverifiable filesystem policies fail closed;
  parent ACL permissions remain constrained by the mode/ACL mask and sticky rule.
  On macOS,
  files cannot have extended ACLs; parents can have deny-only ACLs, but any
  allowing ACL fails closed. Remote filesystems are rejected.

Windows uses the existing current-user file and protected-DACL checks and
rejects final-entry reparse points. Intermediate junctions retain the existing
Windows behavior; this is not a traversal-wide reparse prohibition.
It does not apply Unix root or mode-bit rules. Other
platforms that cannot enforce the policy fail closed. Existing runtime-state
helpers retain their stricter policy; this API is for reading secrets.

`secretref` additionally limits the raw file to **64 KiB**, checking both metadata
and a bounded read so a growing file cannot bypass the limit. It strips all
trailing CR and LF bytes and preserves all other bytes, including spaces and
tabs. It rejects blank results and NUL anywhere. A FIFO or other non-regular
file fails without waiting for a writer.

These rules broaden older `secretref` file support to read-only files, trusted
root-owned mounts, and trusted links. They also reject relative file selectors,
unsafe ancestors, files over 64 KiB, and NUL-containing secrets. Applications
adopting this resolver must explicitly account for their previous limits,
trimming, precedence, and relative-path rules.

## systemd credentials

Have systemd load the credential, then configure its absolute runtime path:

```ini
[Service]
LoadCredential=api-token:/etc/example/api-token
```

```toml
token = { file = "/run/credentials/example.service/api-token" }
```

Use the actual unit name and credential filename. There is no shell expansion,
`$CREDENTIALS_DIRECTORY` expansion, command execution, or implicit credential
lookup in `Ref` strings.

## Diagnostics

`Secret` exposes only `Value` and `Source`. Default formatting (`%s`, `%q`, `%v`,
`%+v`, `%#v`), JSON marshaling, and `slog` redact the resolved value. Access
`Value` explicitly only at the consumer boundary; logging that string directly
will expose it. `Ref` is serializable configuration and can contain a literal
secret, so do not log the configuration itself.

Resolution failures use `*secretref.SourceError`: `Source` names the configured
path or environment selector, while `Reason` contains no secret or selector.
Use `errors.As` to choose whether a log should include that source. Context
cancellation and validation conflicts can return other errors. Removing the
former `Secret.Reason` field makes every failed resolution an error instead of
a success-shaped diagnostic.
