# embedfit invariants

- Count tokens on the formatted string: prefix, source slice, suffix.
- Measure overlap on the source slice. The next window must start after the
  current start.
- Truncation is explicit. `reject` returns an error for a hard cut or a
  dropped tail. `drop_tail` may hard-cut, sets `Truncated` on that span, and
  sets `TailDropped` when source remains.
- Source coordinates refer to the original string. `Prepared.Text` is the
  formatted model input using the prefix and suffix Fit counted. Prepared
  ignores a different pair.
- A separator at the start of the preferred window is a valid soft cut.
  The text after it stays in the fit. Boundaries remain paragraph breaks,
  sentence endings, and ASCII spaces.
- Do not wire this package into `vector.Fill` from here. Fill's prepared
  input belongs to the bounded-fill work and should consume `Prepared`.
- A tokenizer must be monotonic: a longer string has at least as many tokens
  as any prefix of it. A non-empty, non-blank string must count as at least
  one token.
