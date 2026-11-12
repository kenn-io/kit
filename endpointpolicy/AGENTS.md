# Endpoint policy invariants

- This is the shared string policy for integration endpoints; keep it independent
  of embedding configuration and application-specific config or transport code.
- Never resolve DNS or classify names as public/private. The explicit private
  network opt-in trusts names; literal public IPs still fail for plaintext HTTP.
- Preserve loopback, private, link-local unicast, unspecified, IPv4-mapped and
  CGNAT behavior. Strip IPv6 zones for address checks, preserve zone case and
  encode each percent sign in canonical output.
- Subjects affect diagnostics only. Subject `embed` must retain embedconfig's
  existing error text, validation order and wrapped parse errors.
- Reject userinfo, nonempty query/fragment and decoded parent segments. Empty
  query/fragment delimiters remain accepted and removed during canonicalization.
- Origin intentionally checks only scheme/hostname presence. Keep its behavior
  separate from full endpoint validation.
- Property tests use rapid in `property_test.go`; keep them in this package
  so the regular test run covers them.
