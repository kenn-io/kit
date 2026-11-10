---
title: "Checking standalone release archives"
description: "Declare and verify final executable and runtime bundle artifacts before publishing."
last_edited: 2026-11-10
---

# Checking standalone release archives

`kennlint release-artifacts` checks final local archives against a consumer-owned
JSON contract. It catches missing targets, checksum drift, omitted companions,
unsafe layouts, and native binaries built for the wrong OS or architecture.
It reads archives without extracting or executing them and makes no network
requests. The Go API is `go.kenn.io/kit/lint/releasecheck`.

## Declare the distribution

List every supported target explicitly. Supported contract targets are Linux,
macOS (`darwin`), and Windows on `amd64` or `arm64`, with one archive per
OS/architecture. Unix archives use `.tar.gz`; Windows archives use `.zip`.
Omit unsupported targets deliberately and explain the exclusion in your release
documentation. Never derive the contract from the files a build happened to
produce.

Choose the artifact kind for what an end user must install:

| Kind | Meaning | Installer |
| --- | --- | --- |
| `static` | Linux ELF with no interpreter or dynamic imports | Binary-only installer |
| `single` | One executable using only explicitly declared baseline system libraries | Binary-only installer |
| `bundle` | Executable plus declared native libraries or helper executables | Installer that preserves the whole bundle |

`CGO_ENABLED=0` alone does not establish that a binary is static. A macOS or
Windows executable linked against OS libraries is `single`. Libraries loaded
through `dlopen` or `LoadLibrary` belong in `runtime_files` even when ordinary
import tables do not mention them. Put data assets and distribution notices in
`required_files`. All declared members must be nonempty regular files, owner
readable on Unix; Unix executables and helpers must carry executable permission.
Links, traversal,
duplicate paths, and ambiguous Windows member names fail validation. On
case-insensitive targets, names that differ only by case also count as
duplicates, as do canonically equivalent Unicode spellings on macOS.

For example, a genuinely static Linux executable with a notice:

```json
{
  "artifacts": [{
    "name": "example_{version}_linux_amd64.tar.gz",
    "os": "linux",
    "arch": "amd64",
    "executable": "example",
    "kind": "static",
    "baseline": "Current supported Linux amd64; no dynamic runtime dependencies",
    "system_libraries": [],
    "runtime_files": [],
    "required_files": ["LICENSE"]
  }]
}
```

A macOS single executable instead uses `"kind": "single"` and an exact
`system_libraries` list, such as `["/usr/lib/libSystem.B.dylib"]` when that is
the complete import set of the actual executable. Review these names against
the baseline you support; do not copy a list from the build machine.

This bundle example declares a lazily loaded library and its baseline import:

```json
{
  "artifacts": [{
    "name": "example_{version}_darwin_arm64.tar.gz",
    "os": "darwin",
    "arch": "arm64",
    "executable": "example",
    "kind": "bundle",
    "baseline": "Current supported macOS arm64; verify on the oldest supported OS",
    "system_libraries": ["/usr/lib/libSystem.B.dylib"],
    "runtime_files": ["libexample.dylib"],
    "required_files": ["LICENSE", "ThirdPartyNotices.txt"]
  }]
}
```

Every runtime file is inspected for its target and ordinary imports, including
transitive dependencies between declared companions. Recognized native files
cannot silently appear as undeclared payloads. System imports are exact names,
case insensitive on Windows, without wildcards. An ELF interpreter needs its
exact absolute path in `system_libraries`. Loader-relative library references
can match declared members, and bare Windows DLL names match members beside the
importing executable. Libraries may match beside any bundled executable, without
tracing which one loads them. An absolute external dependency cannot be
satisfied by shipping a file with the same basename.

`baseline` records the reviewed OS/libc/runtime policy. It is required prose,
not a compatibility solver. Import names do not establish symbol version floors,
macOS deployment compatibility, Windows runtime availability, or loader search
behavior. Declare those requirements and test them on the supported baseline.
Homebrew, source builds, and custom-prefix installations can keep their own
dependency policies; this opt-in check applies to the standalone assets you
declare.

## Check the final bytes before upload

Finish packaging, create `SHA256SUMS` from those exact archive bytes, then run:

```sh
go run go.kenn.io/kit/cmd/kennlint@<pinned-kit-version> release-artifacts \
  -contract .github/release-artifacts.json -dir dist -version "$VERSION"
```

Replace `<pinned-kit-version>` with the reviewed kit version used by your
workflow. An installed `kennlint` accepts the same command. `{version}` in names
expands to `-version` with one optional leading `v` removed. No glob discovery
or alternate filename fallback occurs. Every selected archive needs exactly one
valid matching SHA-256 entry. Standard `sha256sum` text/binary markers and
`./name` are accepted; unrelated manifest entries may coexist.

An OS matrix leg can pass `-os linux`, `-os darwin`, or `-os windows` to require
every declared architecture for that OS. After aggregation, run the command
again **without `-os`** so a missing matrix leg cannot pass. Upload only archives
from individual legs, then produce one combined checksum manifest. Keep your
existing signing and provenance steps and asset names. Run this check after
any operation that changes archive bytes and before publishing them.

## Smoke the installed layout

After validation, extract a final native archive to a new location and launch
its executable from another working directory. Clear injected loader paths and
preloads. Exercise the features that load native libraries, helper processes,
embedded assets, or plugins, using explicit fixture directories. A version
command alone does not cover lazy dependencies. Keep separately downloaded
models distinct from the release bundle and verify those fixtures too.

`selfupdate.InstallArchive` and binary-only shell installers retain just the
executable. They cannot install a `bundle` correctly. Use a bundle-aware
installation path such as the existing embedding installer, or supply your own
whole-directory installation. The checker does not change installer behavior.

The [kennembed contract](../.github/kennembed-release.json) is a complete
six-target adoption. Its workflow checks each final pair, validates all six
after aggregation, and runs inference from the extracted native archive with
separate model fixtures. Native execution covers the runner's architecture;
cross-built targets receive structural/import checks only. Add native baseline
coverage for each platform claim your own release makes.

The checker audits ordinary PE imports, including ordinal-only dependencies,
ELF dynamic imports and interpreter paths, and thin macOS Mach-O imports.
Delay imports, dynamic loading, ABI symbol floors, and full loader resolution
still require consumer smoke tests. Successful checksums establish consistency
with your manifest; authentication remains the responsibility of your signing
and provenance policy.
