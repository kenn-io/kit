# Endpoint policy

Use one canonicalization and plaintext HTTP rule for integration clients:

```go
endpoint, err := endpointpolicy.CanonicalEndpoint(rawURL, trustPrivateNetwork, "catalog")
```

The final argument labels errors, such as `catalog endpoint must include a
host`. An empty label gives generic errors. `embedconfig.CanonicalEndpoint`
and `embedconfig.Origin` remain compatibility wrappers with label `embed`.

HTTPS and loopback HTTP pass by default. An explicit private-network opt-in
also allows HTTP to private, link-local, unspecified and CGNAT addresses,
and to any DNS name. The name is the operator's trust assertion; this package
does no DNS lookup. Literal public IPs remain refused for HTTP.

Canonicalization lowercases the scheme and host, removes default ports and
trailing slashes, preserves escaped paths and IPv6-zone case, and rejects
userinfo, nonempty queries/fragments and decoded parent path segments.
`Origin` only checks for a scheme and hostname before formatting the origin;
it does not validate the whole endpoint or apply the plaintext policy.

Callers own the opt-in's configuration key, redirects, credential pinning and
transport security. Plain HTTP carries traffic unencrypted on that network.

The Hegel property suite lives in a separate test module to avoid changing
production dependency versions. CI runs it on Linux, macOS and Windows. Run
it locally with `cd endpointpolicy/propertytests && go test ./...`; ordinary
package tests also run with `go test ./endpointpolicy ./embedconfig`.
