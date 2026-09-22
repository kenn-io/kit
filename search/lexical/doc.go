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
// CJK search is off on the zero CJK value. EnableCharacterPhrase or
// EnableChinese turns on a second index that the caller creates and names.
// The ordinary full-text index stays. IndexFor sends a query to the CJK
// index when the text contains Han, Hangul, Hiragana, or Katakana, including
// a mixed query such as "run 搜索". Other queries stay on the ordinary index.
//
// Intentional query syntax uses PrepareAdvanced and is not inferred from
// punctuation. Index and query preparation must use the same identity.
package lexical
