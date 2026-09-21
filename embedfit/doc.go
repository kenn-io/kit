// Package embedfit fits formatted text into a token budget and records where
// each piece came from.
//
// The tokenizer counts the string the model will see, including a role prefix
// or suffix and any heading the caller already added. Splits prefer a
// paragraph boundary, then a sentence boundary, then a word boundary, in the
// last quarter of the window that fits. A hard cut is truncation and happens
// only when the truncation policy allows it.
//
// Prepared is the chunk shape a fill loop can accept without assuming rune
// windows or a list of bare strings. This package does not call vector.Fill.
// The fill entry point is tracked separately and should take Prepared values
// rather than defining a second chunk type.
package embedfit
