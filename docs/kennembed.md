# Installing the local embedding server

Apps share one verified server and installed models under `~/.kenn/embeddings`. Confirmation belongs to each app. Completed installations work offline. Downloading missing files asks for their remaining download size. An optional `Progress` callback reports bytes downloaded against the exact total as data arrives, so apps can draw a progress bar. The installer serializes callers and reserves `.install-*` directories for temporary downloads.

`InstallOptions.Model` selects `embedlocal.TextModel`, the default, or `embedlocal.FaceModel`. Faces download only AuraFace-v1 and YuNet, 260,926,740 bytes, plus the 20 MB server bundle allowance when needed. Text installations prefer the current bundle and reuse an older complete bundle offline when the current bundle is absent. Face installations require the current bundle. Call `Install` before requesting inference. Direct `kennembed` launches also require installation first; they no longer download the text model at startup. The server loads verified, installed models on demand and makes no network requests. Installing another model makes it available to the running server.

Use the same model selector in `embedlocal.Options` when calling `Endpoint`. Face discovery selects kennembed directly. Discovery returns `ErrNotFound` when the running server lacks the requested model, so the caller can offer installation. An older text server still serves text; face discovery returns `RestartRequiredError` with its PID. Restart that process using the newly installed executable.

## Face vectors

`Client.EmbedFaces(ctx, preview, boxes)` sends a JPEG or PNG preview and normalized `{x,y,width,height}` boxes to `POST /v1/faces`. Coordinates start at the top left of the EXIF-oriented preview, before any authored rotation. Box edges allow rounding error up to `1e-6`. The preview may contain at most 40 million pixels and 128 boxes; the encoded request retains the 64 MiB limit. `GET /v1/models` lists models with complete files at the pinned sizes. Loading verifies their SHA-256 digests.

The response preserves every input index. Each embedding is a 512-number unit vector, or JSON null when YuNet finds no face in the box. Decode and inference failures fail the request. The response also carries `model`, `revision`, and `recipe`; persist all three with vectors to distinguish their identity across future model or alignment changes.

AuraFace-v1 is pinned at `af6d057c9b0ec4071d4c49c80e3539258798b609`, and YuNet at `f12e12798e8314f7c074a6656816c048dcc95b7a`. Each box is resized proportionally to a longest side of 320, padded to 640 square, and searched with YuNet at confidence 0.6. The detector matching the requested box with greatest overlap supplies five landmarks for an ArcFace similarity fit. The aligned 112-square image uses BGR planes with `(value-127.5)/128` preprocessing. Bundles carry AuraFace's Apache-2.0 and YuNet's MIT notices.

Native acceptance uses the public astronaut sources and explicit person boxes under `embedserver/testdata/faces`. Fixture staging verifies source sizes and hashes before the server starts. Native tests report maximum same-person and minimum different-person Euclidean distances, and the distance that separates them. On Linux amd64, the original manifest produced a maximum same-person distance of 1.182335 and a minimum different-person distance of 1.237758, so 1.23 separates it. Adding the Scott annotation lowers the minimum different-person distance to 1.196469; use 1.189402 for this expanded corpus. Both Gemini-8 faces return vectors. The obscured EVA-training face returns null.

On the expanded corpus, InsightFace's RGB `(value - 127.5) / 127.5` recipe produced a maximum same-person distance of 1.145075 and a minimum different-person distance of 1.148847. The BGR recipe retains the wider measured separation. These measurements characterize this corpus, not a general identity threshold.

The server holds the shared inference gate while decoding a face preview and processing its boxes. Text requests wait until that photo finishes. Missing or corrupt model assets return HTTP 424 with the installation error; clients can install the requested model and retry.

## Publishing bundles

`internal/embedassets.BundleTag` pins the server independently of ordinary kit updates. The current pin is [v0.48.2-face.4](https://github.com/kenn-io/kit/releases/tag/v0.48.2-face.4), which includes face inference and all six platform archives with `SHA256SUMS`. For future updates, dispatch `kennembed.yml` with the reviewed release tag and update `BundleTag` after all archives and checksums are attached. The following ordinary kit release carries that pin for consumers.

The workflow builds the selected tag's exact source, checks its embedded source revision, and exercises its native executable. It packages Linux, macOS, and Windows for amd64 and arm64 with runtime libraries and distribution notices. Pull requests build and exercise their checked-out candidate as workflow artifacts. No bundle workflow is required for unrelated kit releases.
