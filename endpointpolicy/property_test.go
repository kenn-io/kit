package endpointpolicy_test

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"go.kenn.io/kit/endpointpolicy"
)

// Prefixes the address generator draws from, so policy boundaries are hit
// far more often than uniform random addresses would hit them.
var interestingPrefixes = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.0.0.0/8"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("fc00::/6"),
	netip.MustParsePrefix("169.0.0.0/8"), netip.MustParsePrefix("fe80::/9"),
	netip.MustParsePrefix("100.0.0.0/8"), netip.MustParsePrefix("0.0.0.0/24"),
	netip.MustParsePrefix("::/120"), netip.MustParsePrefix("::ffff:0.0.0.0/96"),
	netip.MustParsePrefix("8.8.0.0/16"), netip.MustParsePrefix("2001:4860::/32"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("ff00::/8"),
	netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0"),
}

// addrGen draws an address inside one of prefixes, with random host bits.
func addrGen(prefixes []netip.Prefix) *rapid.Generator[netip.Addr] {
	return rapid.Custom(func(t *rapid.T) netip.Addr {
		p := rapid.SampledFrom(prefixes).Draw(t, "prefix")
		b := p.Addr().AsSlice()
		for i := range b {
			fixed := min(max(p.Bits()-8*i, 0), 8)
			mask := byte(0xff << (8 - fixed))
			b[i] = b[i]&mask | rapid.Byte().Draw(t, "byte")&^mask
		}
		addr, _ := netip.AddrFromSlice(b)
		return addr
	})
}

func filterPrefixes(v4 bool) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range interestingPrefixes {
		if p.Addr().Is4() == v4 {
			out = append(out, p)
		}
	}
	return out
}

// urlish draws either arbitrary text or a URL assembled from plausible parts.
var urlish = rapid.OneOf(
	rapid.String(),
	rapid.Custom(func(t *rapid.T) string {
		scheme := rapid.SampledFrom([]string{"http", "https", "HTTP", "ftp", ""}).Draw(t, "scheme")
		host := rapid.SampledFrom([]string{
			"localhost", "127.0.0.1", "10.0.0.1", "8.8.8.8", "[::1]", "[fe80::1%eth0]",
			"service", "user@example.test", ":443", "",
		}).Draw(t, "host")
		port := rapid.SampledFrom([]string{"", ":80", ":443", ":8080"}).Draw(t, "port")
		path := rapid.SampledFrom([]string{"", "/", "/v1", "/../", "/%2e%2e", "/a%2Fb/"}).Draw(t, "path")
		tail := rapid.SampledFrom([]string{"", "?", "#", "?q=1", "#f"}).Draw(t, "tail")
		return scheme + "://" + host + port + path + tail
	}),
)

func TestIPAddressPolicyProperty(t *testing.T) {
	// CIDR membership is an independent oracle for the documented address sets.
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	private := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("fe80::/10"),
		netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("0.0.0.0/32"),
		netip.MustParsePrefix("::/128"),
	}
	rapid.Check(t, func(rt *rapid.T) {
		ip := addrGen(interestingPrefixes).Draw(rt, "ip")
		trust := rapid.Bool().Draw(rt, "trust")
		scheme := rapid.SampledFrom([]string{"http", "https"}).Draw(rt, "scheme")
		raw := scheme + "://" + net.JoinHostPort(ip.String(), "8080") + "/v1"
		wantAllowed := scheme == "https"
		for _, prefix := range loopback {
			wantAllowed = wantAllowed || prefix.Contains(ip.Unmap())
		}
		for _, prefix := range private {
			wantAllowed = wantAllowed || trust && prefix.Contains(ip.Unmap())
		}
		got, err := endpointpolicy.CanonicalEndpoint(raw, trust, "catalog")
		if !wantAllowed {
			require.EqualError(rt, err, "catalog plaintext http requires loopback or an explicitly trusted private address")
			assert.Empty(rt, got)
			return
		}
		require.NoError(rt, err)
		assert.Equal(rt, raw, got)
	})
}

func TestCanonicalizationAndIdempotenceProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		segment := rapid.String().Draw(rt, "segment")
		trust := rapid.Bool().Draw(rt, "trust")
		raw := " HTTPS://Example.TEST:443/" + url.PathEscape(segment) + "/// "
		got, err := endpointpolicy.CanonicalEndpoint(raw, trust, "catalog")
		if strings.Contains("/"+segment+"/", "/../") {
			require.EqualError(rt, err, "catalog endpoint path must not contain a parent segment")
			return
		}
		require.NoError(rt, err)
		assert.Equal(rt, "https://example.test"+strings.TrimRight("/"+url.PathEscape(segment), "/"), got)
		again, err := endpointpolicy.CanonicalEndpoint(got, trust, "catalog")
		require.NoError(rt, err)
		assert.Equal(rt, got, again)
	})
}

func TestTrustMonotonicityProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		raw := urlish.Draw(rt, "raw")
		without, strictErr := endpointpolicy.CanonicalEndpoint(raw, false, "catalog")
		with, trustedErr := endpointpolicy.CanonicalEndpoint(raw, true, "catalog")
		if strictErr == nil {
			require.NoError(rt, trustedErr)
			assert.Equal(rt, without, with)
		}
	})
}

func TestMappedIPv4PolicyProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ip := addrGen(filterPrefixes(true)).Draw(rt, "ip")
		trust := rapid.Bool().Draw(rt, "trust")
		_, plainErr := endpointpolicy.CanonicalEndpoint("http://"+ip.String(), trust, "")
		octets := ip.As4()
		for _, host := range []string{"::ffff:" + ip.String(), fmt.Sprintf("::ffff:%x:%x", octets[0:2], octets[2:4])} {
			_, mappedErr := endpointpolicy.CanonicalEndpoint("http://["+host+"]", trust, "")
			assert.Equal(rt, plainErr == nil, mappedErr == nil, host)
		}
	})
}

func TestZonesPreservePolicyAndTextProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ip := addrGen(filterPrefixes(false)).Draw(rt, "ip")
		zone := rapid.StringMatching(`[A-Za-z][A-Za-z0-9_.%]*`).Draw(rt, "zone")
		trust := rapid.Bool().Draw(rt, "trust")
		scheme := rapid.SampledFrom([]string{"http", "https"}).Draw(rt, "scheme")
		base := scheme + "://[" + ip.String() + "]"
		want, baseErr := endpointpolicy.CanonicalEndpoint(base, trust, "catalog")
		encodedZone := strings.ReplaceAll(zone, "%", "%25")
		zoned := scheme + "://[" + ip.String() + "%25" + encodedZone + "]"
		got, err := endpointpolicy.CanonicalEndpoint(zoned, trust, "catalog")
		if baseErr != nil {
			require.EqualError(rt, err, baseErr.Error())
			return
		}
		require.NoError(rt, err)
		assert.Equal(rt, strings.TrimSuffix(want, "]")+"%25"+encodedZone+"]", got)
		if !strings.Contains(zone, "%") {
			bare, bareErr := endpointpolicy.CanonicalEndpoint(scheme+"://["+ip.String()+"%"+zone+"]", trust, "catalog")
			require.NoError(rt, bareErr)
			assert.Equal(rt, got, bare)
		}
		again, err := endpointpolicy.CanonicalEndpoint(got, trust, "catalog")
		require.NoError(rt, err)
		assert.Equal(rt, got, again)
		u := &url.URL{Scheme: scheme, Host: "[" + strings.ToUpper(ip.String()) + "%" + zone + "]"}
		origin, err := endpointpolicy.Origin(u, "catalog")
		require.NoError(rt, err)
		assert.Equal(rt, got, origin)
	})
}

func TestPortsAndWhitespaceProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		port := rapid.IntRange(1, 65535).Draw(rt, "port")
		scheme := rapid.SampledFrom([]string{"http", "https"}).Draw(rt, "scheme")
		host := rapid.SampledFrom([]string{"LOCALHOST", "[::1]", "[FE80::A%25En0]", "SERVICE.example"}).Draw(rt, "host")
		space := strings.Join(rapid.SliceOf(rapid.SampledFrom([]string{" ", "\t", "\n", "\u00a0", "\u2003"})).Draw(rt, "space"), "")
		authority := host
		defaultPort := scheme == "http" && port == 80 || scheme == "https" && port == 443
		if !defaultPort {
			authority += fmt.Sprintf(":%d", port)
		}
		want := scheme + "://" + strings.ReplaceAll(strings.ToLower(authority), "en0", "En0") + "/V1%2fItem"
		raw := space + strings.ToUpper(scheme) + "://" + host + fmt.Sprintf(":%d/V1%%2fItem///", port) + space
		got, err := endpointpolicy.CanonicalEndpoint(raw, true, "catalog")
		require.NoError(rt, err)
		assert.Equal(rt, want, got)
	})
}

func TestDecodedParentSegmentsProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		dot := rapid.SampledFrom([]string{".", "%2e", "%2E"})
		sep := rapid.SampledFrom([]string{"/", "%2f", "%2F"})
		path := "/safe" + sep.Draw(rt, "sep") + dot.Draw(rt, "dot") + dot.Draw(rt, "dot") + sep.Draw(rt, "sep") + "child"
		trust := rapid.Bool().Draw(rt, "trust")
		_, err := endpointpolicy.CanonicalEndpoint("https://example.test"+path, trust, "catalog")
		require.EqualError(rt, err, "catalog endpoint path must not contain a parent segment")
	})
}

func TestStructuralFaultsAndPrecedenceProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		path := url.PathEscape(rapid.String().Draw(rt, "path"))
		fault := rapid.SampledFrom([]struct{ raw, message string }{
			{"ftp://user@example.test/../?q=1", "endpoint must use http or https"},
			{"https://:443/" + path, "endpoint must include a host"},
			{"https://@example.test/" + path + "?q=1", "endpoint must not include userinfo"},
			{"http://example.test/../?q=" + path, "endpoint must not include a query or fragment"},
			{"http://example.test/#value", "endpoint must not include a query or fragment"},
			{"http://example.test/../", "endpoint path must not contain a parent segment"},
		}).Draw(rt, "fault")
		trust := rapid.Bool().Draw(rt, "trust")
		got, err := endpointpolicy.CanonicalEndpoint(fault.raw, trust, "")
		require.EqualError(rt, err, fault.message)
		assert.Empty(rt, got)
	})
}

func TestSubjectDiagnosticsProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		raw := urlish.Draw(rt, "raw")
		subject := rapid.OneOf(rapid.String(), rapid.Map(rapid.SliceOf(rapid.Byte()), func(b []byte) string { return string(b) })).Draw(rt, "subject")
		prefix := subject
		if prefix != "" {
			prefix += " "
		}
		trust := rapid.Bool().Draw(rt, "trust")
		want, wantErr := endpointpolicy.CanonicalEndpoint(raw, trust, "")
		got, err := endpointpolicy.CanonicalEndpoint(raw, trust, subject)
		assert.Equal(rt, want, got)
		if wantErr == nil {
			require.NoError(rt, err)
		} else {
			require.EqualError(rt, err, prefix+wantErr.Error())
		}
		u := &url.URL{Scheme: rapid.String().Draw(rt, "scheme"), Host: raw}
		want, wantErr = endpointpolicy.Origin(u, "")
		got, err = endpointpolicy.Origin(u, subject)
		assert.Equal(rt, want, got)
		if wantErr == nil {
			require.NoError(rt, err)
		} else {
			require.EqualError(rt, err, prefix+wantErr.Error())
		}
	})
}

func TestOriginProjectionProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		scheme := rapid.SampledFrom([]string{"https", "HTTPS", "http", "HtTp", "ftp"}).Draw(rt, "scheme")
		host := rapid.SampledFrom([]string{"EXAMPLE.TEST", "127.0.0.1", "[FE80::1%En0]"}).Draw(rt, "host")
		port := rapid.SampledFrom([]string{"80", "443", "8080"}).Draw(rt, "port")
		text := rapid.String()
		u := &url.URL{
			Scheme: scheme, Host: host + ":" + port,
			Path: text.Draw(rt, "path"), RawPath: text.Draw(rt, "rawPath"),
			RawQuery: text.Draw(rt, "query"), Fragment: text.Draw(rt, "fragment"),
			Opaque: text.Draw(rt, "opaque"), ForceQuery: rapid.Bool().Draw(rt, "forceQuery"),
			User: url.UserPassword(text.Draw(rt, "user"), text.Draw(rt, "password")),
		}
		wantHost := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(host), "%", "%25"), "en0", "En0")
		defaultPort := scheme == "https" && port == "443" || scheme == "http" && port == "80"
		if !defaultPort {
			wantHost += ":" + port
		}
		want := strings.ToLower(scheme) + "://" + wantHost
		got, err := endpointpolicy.Origin(u, "catalog")
		require.NoError(rt, err)
		assert.Equal(rt, want, got)
	})
}
