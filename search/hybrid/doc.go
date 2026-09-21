// Package hybrid runs mapped search queries and fuses their rank order.
//
// Each leg uses a query built by a backend package, such as sqlitefts or
// sqlitevec, and the same sqlquery value those packages return for direct
// execution. This package does not build SQL and it does not merge
// generations. vector.Merge remains the generation policy.
//
// A leg that returns as many rows as its candidate limit has a full window.
// That is not exhaustion and does not prove another eligible document is
// absent. Result limits belong after the caller checks eligibility.
package hybrid
