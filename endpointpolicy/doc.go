// Package endpointpolicy canonicalizes HTTP integration endpoints and applies
// an explicit private-network plaintext policy without resolving DNS names.
//
// HTTPS and loopback HTTP are allowed by default. An explicit opt-in allows
// HTTP to private, link-local, unspecified and carrier-grade NAT IPs, and to
// any DNS name. A name is not classified as public or private: the opt-in is
// the operator's statement that the name is on a network they trust. Public
// literal IPs remain refused even with the opt-in.
//
// Callers own configuration keys, transport behavior, redirect handling and
// credential pinning. This package validates and formats endpoint strings;
// it does not establish that a configured network is safe.
package endpointpolicy
