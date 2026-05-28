# Migration Notes

`go.kenn.io/kit/selfupdate` is intended to replace the duplicated
`internal/update` packages in `agentsview` and `msgvault`.

## Minimal Integration

Create a `selfupdate.Client` in each CLI update command:

```go
client := selfupdate.Client{
	Owner:          "wesm",
	Repo:           "agentsview", // or "msgvault"
	BinaryName:     "agentsview", // or "msgvault"
	CurrentVersion: version,
	CacheDir:       appCacheDir,
}
```

Use `client.Check(ctx, selfupdate.CheckOptions{Force: force})` where the
current command calls `CheckForUpdate`. A nil result still means no update is
available. If `info.NeedsRefetch()` is true and the user chooses to install,
call `Check` again with `Force: true` before installing.

Use `client.Install(ctx, info, selfupdate.InstallOptions{Progress: progress})`
where the current command calls `PerformUpdate`. CLI output, config loading,
confirmation prompts, and command wiring should stay in the application.

The current agentsview and msgvault CLI release workflows publish archives plus
`SHA256SUMS`, so the minimal migration keeps that checksum-based integrity
check. Repos that already publish signed update metadata, or add it later, can
also configure `TrustedPublicKeys` or `RequireSignature`. Signed releases should
publish a detached Ed25519 signature asset named `<asset>.sha256.sig` or
`<asset>.sig`; it must sign the canonical `selfupdate.SignaturePayload`
metadata for the archive, including owner, repo, version, asset name, target
platform, and lowercase SHA-256 checksum.

## App-Specific Details

`agentsview` can pass its existing cache directory directly. `msgvault` should
continue using its app home directory as `CacheDir`; the kit package writes
`update_check.json` with `0600` permissions.

The default asset naming convention matches both source packages:
`<binary>_<version>_<goos>_<goarch>.tar.gz`, with `.zip` on Windows. Repos with
different release asset names can provide `Client.AssetName`.
