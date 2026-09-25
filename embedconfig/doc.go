// Package embedconfig holds reusable embedding settings.
//
// Applications own files, environment, flags, secret resolution, and
// persistence. This package owns the value types, defaults, and validation
// those applications were copying.
//
// The types stay separate on purpose. Model, role, and input settings can
// change vectors or force a rebuild. Batch size and timeouts do not.
// embedmodel.Descriptor combines them and computes the identities.
//
// Dimensions and truncation have no default. Batch size defaults to 64
// items and the transport timeout defaults to 30 seconds because those are
// the values the duplicated HTTP clients already used. Deployments that use 32 or 128 items, or a 45 second timeout,
// set those values explicitly.
package embedconfig
