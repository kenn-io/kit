// Package lexical prepares literal, phrase, and analyzer-specific full-text
// queries and records the analyzer identity that must match the index.
//
// Three identities stay distinct. Literal quoting does not segment text.
// Character-phrase analysis preserves adjacency of Han, Hiragana, Katakana,
// and Hangul characters; it is not morphological analysis. Chinese
// segmentation is performed by a caller-supplied segmenter whose runtime and
// dictionary fingerprint is part of the identity. This package does not
// install that runtime.
//
// Intentional query syntax uses PrepareAdvanced and is not inferred from
// punctuation. Index and query preparation must use the same identity.
package lexical
