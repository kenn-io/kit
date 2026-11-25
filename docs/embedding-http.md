---
type: Guide
title: "Embedding HTTP contract"
description: "Implement and check a Kit embedding service."
generated: { by: codex/gpt-6.2-astra, at: "2026-11-25T22:14:38Z" }
---

# Embedding HTTP contract

A Kit embedding service accepts `POST /v1/embed` with a JSON body and returns one vector per input. It accepts requests of at least 64 MiB. Kit's own server limits requests to 64 MiB.

```json
{"model":"example-model","dimensions":768,"encoding_format":"float","inputs":[{"role":"query","parts":[{"text":"search text"}]},{"role":"document","parts":[{"media_type":"image/png","data":"<base64>"},{"text":"caption"}]}]}
```

`model` and nonempty `inputs` are required. Each input has a `role`, either `query` or `document`, and nonempty `parts`. Each part has either a nonempty `text` string or a `media_type` and nonempty standard base64 `data`. Parts make one vector in their given order. The service embeds text as given and adds no prefix or suffix. A caller can apply its model's role formatting before sending text. `dimensions` is optional; zero or an omitted value selects the service default. `encoding_format` is `float` by default or `base64` for little-endian float32 bytes.

A successful response has status 200 and a `model` and `data` array. Each item has an `index` naming its input position and an `embedding` array or base64 string. Vectors contain finite float32 values and have the requested number of dimensions. A vector must be the same whether its input is sent alone or in a batch. Optional `object` and `usage` fields are ignored.

Errors use `{"error":{"code":"...","message":"..."}}`. Return 400 `invalid_request` for malformed requests, 404 `model_not_found` for unknown models, 405 `method_not_allowed` with `Allow: POST`, 413 `input_too_long` for oversized requests or inputs, and 415 `unsupported_content` for media the service cannot embed. Use a 5xx status for other failures. A text-only service conforms if it returns 415 for media. Decode invalid UTF-8 and lone surrogates as replacement characters and keep the last duplicate JSON member.

A service may also offer the OpenAI text-only `POST /v1/embeddings` endpoint. Kit's own server offers it and preserves its existing behavior.

## Run the check

```sh
go run go.kenn.io/kit/cmd/embedcheck -url https://example.com/v1 -model example-model -dims 768
```

Use `-api-key-env NAME` to read a bearer token from an environment variable. Repeat `-media path` to require support for each media file. Without media files the check accepts either a vector or `unsupported_content` for a PNG. Add `-openai` to compare the OpenAI endpoint with the Kit endpoint. The check exercises the HTTP contract and output consistency; it cannot judge whether vectors represent the input well or detect hidden text transformations.

## Implement a service

A third-party service can accept the request above, send each input's parts to its embedding provider, and return the resulting vectors in the specified order. For example, an AWS-backed service can translate each part into the provider's request, map unsupported media to 415, and expose its endpoint at `https://example.com/v1/embed`. Run the check with its URL, model, and dimensions before configuring a Kit client with `api = "kit"` and `base_url = "https://example.com/v1"`.
