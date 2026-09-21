// Package embedmodel describes a vector space, an indexed-input recipe, and
// the content those inputs carry.
//
// Vector-space identity, input-recipe identity, and lexical-analyzer identity
// answer different questions. A chunk-window change rebuilds inputs inside
// the same comparison space. A dictionary change rebuilds a lexical index
// without changing vectors. An API key, timeout, or batch size changes neither.
//
// Content is not a bare list of strings. Text is the first encoded form.
// Image and file parts remain valid descriptions so callers can keep those
// paths, and the shared text client reports that it does not encode them.
// Source spans are coordinates into the caller's source, not into the
// formatted request.
package embedmodel
