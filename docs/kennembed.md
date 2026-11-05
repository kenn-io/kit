# Installing the local embedding server

Apps share one verified server and pinned model under `~/.kenn/embeddings`. Confirmation belongs to each app. Completed installations work offline. Downloading missing files asks for their remaining download size. An optional `Progress` callback reports bytes downloaded against the exact total as data arrives, so apps can draw a progress bar. The installer serializes callers and reserves `.install-*` directories for temporary downloads.

Call `embedlocal.Inspect(ctx, embedlocal.Options{DataDir: dir})` before offering a download. It reports `OllamaAvailable`, `Installing`, and approximate `DownloadBytes` independently. The byte count adds each incomplete model file's pinned size and a 20 MB allowance when the server bundle needs replacement. Zero means the pinned files are present by type and size; `Install` still verifies model contents, including same-size corruption.

Inspection checks the install lock with a shared reader that coexists with other inspections. `Installing` means an installer holds the exclusive lock and has a staging directory for approved work. Waiting for confirmation and leftover staging from an interrupted install report idle. Inspection leaves missing directories absent and existing files untouched. The snapshot is advisory because another installer may publish files afterward. The only network request probes local Ollama for `nomic-embed-text`, with a 500 millisecond deadline and redirects refused. Set `SkipOllama` to inspect files alone. Caller cancellation returns an error. Apps keep ownership of their enabled setting, configured endpoint, provider labels, and indexing progress.

## Publishing bundles

`internal/embedassets.BundleTag` pins the server independently of ordinary kit updates. The initial pin is [v0.47.1](https://github.com/kenn-io/kit/releases/tag/v0.47.1). Its bundles must be published before consumers use the installer.

Run `gh workflow run kennembed.yml --repo kenn-io/kit --field tag=v0.47.1` to build and attach bundles to an existing release. The workflow builds that tag's exact source, checks its embedded source revision, and exercises its native executable. It packages Linux, macOS, and Windows for amd64 and arm64 with runtime libraries and distribution notices, then uploads six archives and `SHA256SUMS`. Pull requests build the pinned bundles as workflow artifacts for inspection and backfill.

Publish a new tag's bundles before changing `BundleTag`. Keep its server, model pins, and runtime filenames aligned. No bundle workflow is required for unrelated kit releases.
