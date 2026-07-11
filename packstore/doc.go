// Package packstore provides a mixed loose-and-packed content-addressed store.
//
// Packstore owns physical storage, validation, and crash-ordered maintenance.
// Applications retain catalog membership and product reachability behind the
// Resolver and Catalog interfaces. A file or pack entry is never sufficient by
// itself to grant read authority.
//
// Physical storage operations are supported on Unix and Windows. Other Go
// targets compile but fail closed because their file APIs do not provide the
// atomic no-follow and nonblocking opens required for race-safe content access.
package packstore
