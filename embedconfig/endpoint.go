package embedconfig

import (
	"net/url"

	"go.kenn.io/kit/endpointpolicy"
)

// CanonicalEndpoint returns the identity and request form of raw.
// The result uses a lowercase scheme and host, drops the default port, and
// drops a trailing slash. An IPv6 zone is preserved as written, not
// lowercased, and each percent sign in it is encoded as %25.
// Userinfo and nonempty query or fragment are rejected so a credential or a
// cache-buster cannot hide inside the endpoint.
// Plaintext HTTP is limited to loopback unless trustPrivateNetwork is set.
// With that opt-in, private, link-local, unspecified, and carrier-grade NAT
// addresses are allowed, and so is a DNS name. A name is not classified as
// public or private. The opt-in is the caller's statement that the name is
// on a network they trust. Without the opt-in, a name other than localhost
// is rejected. The zone is removed before the address check.
// A parent step is a decoded path piece that is exactly "..". A name that
// contains two dots, such as "v1..2", is kept.
func CanonicalEndpoint(raw string, trustPrivateNetwork bool) (string, error) {
	return endpointpolicy.CanonicalEndpoint(raw, trustPrivateNetwork, "embed")
}

// Origin returns the canonical scheme and host of u, without a path.
func Origin(u *url.URL) (string, error) {
	return endpointpolicy.Origin(u, "embed")
}
