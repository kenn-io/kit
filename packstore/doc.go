// Package packstore provides a mixed loose-and-packed content-addressed store.
//
// Packstore owns physical storage, validation, and crash-ordered maintenance.
// Applications retain catalog membership and product reachability behind the
// Resolver and Catalog interfaces. A file or pack entry is never sufficient by
// itself to grant read authority.
package packstore
