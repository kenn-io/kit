---
type: Guide
title: "Embedding HTTP contract"
description: "Implement and check a Kit embedding service."
generated: { by: unattributed, at: "2026-11-26T01:18:09Z" }
---

# Embedding HTTP contract

This is the HTTP form of Kit's embedding request. A service that follows it can embed text, images, and files for any Kit client. You can build one in front of any embedding provider and check it with `embedcheck` before you connect a client.

A service has a base URL ending in `/v1`, such as `https://embed.example.com/v1`. It must serve `POST {base}/embed`. It may also serve OpenAI's text-only `POST {base}/embeddings`. `embedcheck` checks only `/embed`.

## Request

The client sends `POST {base}/embed` with `Content-Type: application/json`. When it has an API key, it sends `Authorization: Bearer <key>`. The service must accept request bodies of at least 64 MiB. Kit's own server accepts up to exactly 64 MiB.

```json
{
  "model": "example-model",
  "dimensions": 768,
  "encoding_format": "float",
  "inputs": [
    {"role": "query", "text": "beach at sunset"},
    {"role": "document", "media_type": "image/png", "data": "<base64>"}
  ]
}
```

| Field | Rule |
|---|---|
| `model` | Required. A model name the service serves. |
| `inputs` | Required and nonempty. Each input becomes one vector. A request may mix `query` and `document` inputs. |
| `inputs[].role` | Required. `query` or `document`. |
| `inputs[].text` | Text to embed. It needs at least one character that is not whitespace, a format character, or a control character. |
| `inputs[].media_type` | The media type of `data`, such as `image/png` or `application/pdf`. It must not be blank. |
| `inputs[].data` | The media bytes as standard base64. |
| `dimensions` | Optional. Zero or absent selects the service's default size. Kit clients always send their configured size. |
| `encoding_format` | Optional. `float`, the default, or `base64`. |

Each input carries exactly one payload: `text`, or `media_type` with `data`. An input with both payloads, neither payload, or a media type without data is invalid. An empty string or `null` counts as absent. To embed a photo and its caption, send them as two inputs and store two vectors. Kit clients batch text inputs and send each media input in its own request.

The service sends text as given and must not add a prefix, suffix, or task instruction. A model may truncate text at its input window. A caller that needs its model's role formatting applies it before sending. The role is there for providers that take a task type.

The service decodes JSON leniently: invalid UTF-8 and lone surrogates become U+FFFD, and the last of duplicate members wins.

## Response

A successful response has status 200 and a JSON body:

```json
{"model": "example-model", "data": [{"index": 0, "embedding": [0.12, -0.03]}, {"index": 1, "embedding": [0.08, 0.41]}]}
```

- `data` has exactly one item per input.
- Each item's `index` is the position of its input in `inputs`. Items may be in any order when every item has an `index`. Do not mix indexed and unindexed items.
- `embedding` is a JSON array of numbers for `float`. For `base64`, it is a base64 string of little-endian float32 values.
- Each vector has exactly the requested number of dimensions. Its values are finite float32 values and are not all zero.
- An input gets the same vector within float32 rounding whether it is sent alone or in a batch, and with either encoding. Each component may differ by at most `1e-5 + 1e-5 * max(abs(a), abs(b))`.
- Kit ignores the optional `object` and `usage` fields.

## Errors

Every error response has a JSON body:

```json
{"error": {"code": "invalid_request", "message": "invalid request"}}
```

Use these statuses and codes. Kit clients classify errors by them. "Retryable" means a Kit client retries only when the caller's retry settings allow it; by default it does not retry.

| Status | `code` | When | Kit client |
|---|---|---|---|
| 400 | `invalid_request` | The body is malformed or breaks a request rule, the service cannot serve the requested size, or the service recognizes the provider's rejection as a request or configuration error, such as an unknown model | Configuration error; not retried |
| 400 | `content_policy_violation` | The provider refused an input under its content policy | Input rejected; not retried |
| 404 | `model_not_found` | The service does not serve `model` | Configuration error; not retried |
| 405 | `method_not_allowed` | The method is not `POST`; also send `Allow: POST` | Configuration error; not retried |
| 413 | `input_too_long` | The body exceeds the service's limit, or an input exceeds the model's input limit and the service does not truncate it | Input rejected; not retried |
| 415 | `unsupported_content` | The service cannot embed a media input | Unsupported content; not retried |
| 424 | `model_unavailable` | The service's own model cannot load | Configuration error; not retried |
| 429 | `rate_limited` | The provider is rate limiting the service; send `Retry-After` in whole seconds when the provider gave a delay | Retryable |
| 500 | `internal_error` | The service produced an invalid result | Retryable |
| 502 | `upstream_credentials` | The provider rejected the service's credentials | Credentials error; not retried |
| 503 | `unavailable` | Any other provider or service failure, including a provider timeout and a provider rejection the service cannot classify | Retryable |

A Kit client also treats 401 and 403 as rejected credentials, except a 403 with code `model_not_found`, which is a configuration error. Neither is retried. It treats 408 and any other 5xx as retryable. It reads `Retry-After` as seconds or an HTTP date, and when its retry settings retry a status, it waits that long, up to their cap. Use the codes above: Kit reads a 400, 413, or 422 by its code, then by its message, and one it cannot classify is unknown and not retried.

A service may close the connection without a response once the caller has disconnected. While the caller is waiting, a failure gets a status from the table.

A text-only service conforms when it embeds text and returns 415 `unsupported_content` for every media input.

## OpenAI text route

A service may also serve `POST {base}/embeddings` for OpenAI-format callers. In Go, `embedserver.Handler` serves both routes from the same backend: a path ending in `/embed` takes the Kit request, and a path ending in `/embeddings` takes this one.

```json
{"model": "example-model", "dimensions": 768, "encoding_format": "float", "input": ["first text", "second text"], "input_type": "query"}
```

- `input` is a string or an array of strings. Each string becomes one text input.
- `input_type: "query"` gives every input the query role. Any other value, or none, gives the document role.
- `model`, `dimensions`, and `encoding_format` follow the Kit request's rules, and so do the error statuses and codes.
- The route takes text only. Any other `input`, including media, gets 400 `invalid_request`.

`Handler` answers with OpenAI's list shape and no `usage`:

```json
{"object": "list", "model": "example-model", "data": [{"object": "embedding", "index": 0, "embedding": [0.12, -0.03]}]}
```

## Kit's built-in server

kennembed serves the built-in text model at `/v1/embed` through `embedserver.Handler`. It keeps its own `/v1/embeddings` handler, which still returns `usage`. The model accepts sizes from 1 to 768, defaults to 768, and returns 415 for media on `/v1/embed`. Its text window is 2,048 tokens, counting the start and end tokens. Both routes truncate longer text at that window, so neither returns 413 for long text.

## Run the check

`embedcheck` sends requests to a running service and reports each rule the service breaks. It needs Go and network access to the service:

```sh
go run go.kenn.io/kit/cmd/embedcheck@latest -url https://embed.example.com/v1 -model example-model -dims 768
```

| Flag | Meaning |
|---|---|
| `-url` | Required. The base URL ending in `/v1`. The check posts to `{url}/embed`. Local and private addresses are allowed. |
| `-model` | Required. A model the service serves. |
| `-dims` | Required. The vector size to request, greater than zero. |
| `-api-key-env NAME` | Read the bearer token from environment variable `NAME`. |
| `-media path` | Require the service to embed this file. Repeat for each file. The file extension sets the media type, for example `.png` or `.pdf`. |

The check requires:

- A text query and a text document embed together in one request. Each vector matches the vector from sending that input alone within the response tolerance.
- The same two-input batch, sent once as `float` and once as `base64`, returns a JSON array for every item in `float` and a string for every item in `base64`, with values matching within the response tolerance.
- Every successful response has status 200 and exactly one item per input. Each item has a unique index within the input range.
- Every vector has the requested size, finite values, and at least one nonzero value.
- Each `-media` file embeds. With no `-media` files, a small PNG either embeds or returns 415 `unsupported_content`.
- An unknown model returns 404 `model_not_found`.
- A malformed body, empty `inputs`, and an unknown role each return 400 `invalid_request`. So does an input with both text and media, an input with neither payload, a media type without data, and media with invalid base64.
- `GET` returns 405 `method_not_allowed` with `Allow: POST`.

It prints one line per broken rule to standard error. It exits 0 when every rule passes, 1 when any rule fails, and 2 when a flag is missing or invalid, the key variable is unset, or a media file cannot be read or has an unknown extension.

The check covers the request and response shape, indexes, sizes, encodings, batching, declared media support, and the request errors above. It does not test vector quality, hidden changes to input text, input or body size limits, rate limits, or provider failures. The `/embeddings` route is optional, and the check does not call it.

## Build a service

A Go service can wrap any `embedclient.Backend` in `embedserver.Handler` and mount it under `{base}/` to serve both routes. This example puts a service at `https://embed.example.com/v1` in front of a hosted embedding provider, such as one running on AWS:

1. Serve `POST /v1/embed`. Validate the request and return 400 or 404 as in the error table before calling the provider.
2. Send each input's one payload to the provider, text or media, with no added text. Pass the role if the provider takes a task type.
3. Return 415 `unsupported_content` for any media type the provider cannot embed.
4. Translate provider failures with the error table. A provider rate limit becomes 429 with `Retry-After`. A refused provider key becomes 502 `upstream_credentials`. A provider rejection you recognize as a request or configuration error becomes 400 `invalid_request`. Anything else becomes 503 `unavailable`.
5. Return one vector per input with its `index`, in the requested size and encoding.
6. Run `embedcheck` against the service. Add `-media` with a sample of each file type it supports.

Then point a Kit embedder at it:

```toml
api = "kit"
base_url = "https://embed.example.com/v1"
model = "example-model"
dims = 768
api_key = { env = "EMBED_API_KEY" }
```
