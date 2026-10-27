# Embedserver invariants

- Keep the pinned assets, tokenizer, and Ollama parity fixture in sync.
- The ONNX Runtime table indices require version 1.23.2 and API 23.
- On shutdown, close HTTP connections, wait for inference, then close the model and remove its runtime record.
