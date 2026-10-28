# Installing the local embedding server

Call `embedlocal.Install` with the shared embeddings directory, conventionally `~/.kenn/embeddings`, and an app-owned `Confirm` callback. The callback receives approximate missing download bytes before any network access. A fresh installation needs about 275 MB of model files plus a 20 MB allowance for the executable and native runtime. Repairs count only the missing files. A missing callback, refusal, or cancellation returns an error.

Pass `Installation.Executable` to the launcher with `--data-dir Installation.DataDir`. Both paths are absolute. Apps sharing the directory reuse `bin/v0.47.1/kennembed` or `kennembed.exe` on Windows, plus the same pinned model cache. Completed installations work offline. The installer serializes callers, repairs missing files or Unix execute permission, and reserves `.install-*` directories for temporary downloads.

## Publishing bundles

`internal/embedassets.BundleTag` pins the server independently of ordinary kit updates. The initial pin is [v0.47.1](https://github.com/kenn-io/kit/releases/tag/v0.47.1). Its bundles must be published before consumers use the installer.

Run `gh workflow run kennembed.yml --repo kenn-io/kit --field tag=v0.47.1` to build and attach bundles to an existing release. The workflow builds that tag's exact source, checks its embedded source revision, and exercises its native executable. It packages Linux, macOS, and Windows for amd64 and arm64 with runtime libraries and distribution notices, then uploads six archives and `SHA256SUMS`. Pull requests build the pinned bundles as workflow artifacts for inspection and backfill.

Publish a new tag's bundles before changing `BundleTag`. Keep its server, model pins, and runtime filenames aligned. No bundle workflow is required for unrelated kit releases.
