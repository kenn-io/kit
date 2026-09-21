package embedconfig

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// CanonicalEndpoint returns the identity and request form of raw.
// The result uses a lowercase scheme and host, drops the default port, and
// drops a trailing slash. Userinfo, query, and fragment are rejected so a
// credential or a cache-buster cannot hide inside the endpoint.
// Plaintext HTTP is limited to loopback unless trustPrivateNetwork is set,
// in which case private, link-local, unspecified, and carrier-grade NAT
// addresses are also allowed. A DNS name other than localhost is never
// treated as private.
func CanonicalEndpoint(raw string, trustPrivateNetwork bool) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("embed endpoint is invalid: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("embed endpoint must use http or https")
	}
	if parsed.Host == "" {
		return "", errors.New("embed endpoint must include a host")
	}
	if parsed.User != nil {
		return "", errors.New("embed endpoint must not include userinfo")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("embed endpoint must not include a query or fragment")
	}
	if strings.Contains(parsed.EscapedPath(), "..") {
		return "", errors.New("embed endpoint path must not contain a parent segment")
	}
	if parsed.Scheme == "http" && !plaintextAllowed(parsed.Hostname(), trustPrivateNetwork) {
		return "", errors.New("embed plaintext http requires loopback or an explicitly trusted private address")
	}
	return schemeHost(parsed) + strings.TrimRight(parsed.EscapedPath(), "/"), nil
}

// Origin returns the canonical scheme and host of u, without a path.
func Origin(u *url.URL) (string, error) {
	if u == nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("embed endpoint origin is missing")
	}
	return schemeHost(u), nil
}

func schemeHost(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return strings.ToLower(u.Scheme) + "://" + host
}

func plaintextAllowed(host string, trustPrivateNetwork bool) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	if !trustPrivateNetwork {
		return false
	}
	return ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || isCGNAT(ip)
}

func isCGNAT(ip net.IP) bool {
	ip = ip.To4()
	return ip != nil && ip[0] == 100 && ip[1]&0xc0 == 0x40
}
