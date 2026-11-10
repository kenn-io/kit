package propertytests_test

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/endpointpolicy"
)

func TestIPAddressPolicy(t *testing.T) {
	// CIDR membership is an independent oracle for the documented address sets.
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	private := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("fe80::/10"),
		netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("0.0.0.0/32"),
		netip.MustParsePrefix("::/128"),
	}
	hegel.Test(t, func(ht *hegel.T) {
		ip := hegel.Draw(ht, hegel.IPAddresses())
		trust := hegel.Draw(ht, hegel.Booleans())
		scheme := hegel.Draw(ht, hegel.SampledFrom([]string{"http", "https"}))
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
			require.EqualError(ht, err, "catalog plaintext http requires loopback or an explicitly trusted private address")
			assert.Empty(ht, got)
			return
		}
		require.NoError(ht, err)
		assert.Equal(ht, raw, got)
	}, hegel.WithDatabase(t.TempDir()), hegel.WithTestCases(1000))
}

func TestCanonicalizationAndIdempotence(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		segment := hegel.Draw(ht, hegel.Text())
		trust := hegel.Draw(ht, hegel.Booleans())
		raw := " HTTPS://Example.TEST:443/" + url.PathEscape(segment) + "/// "
		got, err := endpointpolicy.CanonicalEndpoint(raw, trust, "catalog")
		if strings.Contains("/"+segment+"/", "/../") {
			require.EqualError(ht, err, "catalog endpoint path must not contain a parent segment")
			return
		}
		require.NoError(ht, err)
		assert.Equal(ht, "https://example.test"+strings.TrimRight("/"+url.PathEscape(segment), "/"), got)
		again, err := endpointpolicy.CanonicalEndpoint(got, trust, "catalog")
		require.NoError(ht, err)
		assert.Equal(ht, got, again)
	}, hegel.WithDatabase(t.TempDir()))
}

func TestTrustMonotonicity(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		raw := hegel.Draw(ht, hegel.OneOf(hegel.Text(), hegel.URLs()))
		without, strictErr := endpointpolicy.CanonicalEndpoint(raw, false, "catalog")
		with, trustedErr := endpointpolicy.CanonicalEndpoint(raw, true, "catalog")
		if strictErr == nil {
			require.NoError(ht, trustedErr)
			assert.Equal(ht, without, with)
		}
	}, hegel.WithDatabase(t.TempDir()))
}

func TestMappedIPv4Policy(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		ip := hegel.Draw(ht, hegel.IPAddresses().IPv4())
		trust := hegel.Draw(ht, hegel.Booleans())
		_, plainErr := endpointpolicy.CanonicalEndpoint("http://"+ip.String(), trust, "")
		octets := ip.As4()
		for _, host := range []string{"::ffff:" + ip.String(), fmt.Sprintf("::ffff:%x:%x", octets[0:2], octets[2:4])} {
			_, mappedErr := endpointpolicy.CanonicalEndpoint("http://["+host+"]", trust, "")
			assert.Equal(ht, plainErr == nil, mappedErr == nil)
		}
	}, hegel.WithDatabase(t.TempDir()))
}

func TestZonesPreservePolicyAndText(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		ip := hegel.Draw(ht, hegel.IPAddresses().IPv6())
		zone := hegel.Draw(ht, hegel.FromRegex(`[A-Za-z][A-Za-z0-9_.%]*`, true))
		trust := hegel.Draw(ht, hegel.Booleans())
		scheme := hegel.Draw(ht, hegel.SampledFrom([]string{"http", "https"}))
		base := scheme + "://[" + ip.String() + "]"
		want, baseErr := endpointpolicy.CanonicalEndpoint(base, trust, "catalog")
		encodedZone := strings.ReplaceAll(zone, "%", "%25")
		zoned := scheme + "://[" + ip.String() + "%25" + encodedZone + "]"
		got, err := endpointpolicy.CanonicalEndpoint(zoned, trust, "catalog")
		if baseErr != nil {
			require.EqualError(ht, err, baseErr.Error())
			return
		}
		require.NoError(ht, err)
		assert.Equal(ht, strings.TrimSuffix(want, "]")+"%25"+encodedZone+"]", got)
		if !strings.Contains(zone, "%") {
			bare, bareErr := endpointpolicy.CanonicalEndpoint(scheme+"://["+ip.String()+"%"+zone+"]", trust, "catalog")
			require.NoError(ht, bareErr)
			assert.Equal(ht, got, bare)
		}
		again, err := endpointpolicy.CanonicalEndpoint(got, trust, "catalog")
		require.NoError(ht, err)
		assert.Equal(ht, got, again)
		u := &url.URL{Scheme: scheme, Host: "[" + strings.ToUpper(ip.String()) + "%" + zone + "]"}
		origin, err := endpointpolicy.Origin(u, "catalog")
		require.NoError(ht, err)
		assert.Equal(ht, got, origin)
	}, hegel.WithDatabase(t.TempDir()))
}

func TestPortsAndWhitespace(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		port := hegel.Draw(ht, hegel.Integers(1, 65535))
		scheme := hegel.Draw(ht, hegel.SampledFrom([]string{"http", "https"}))
		host := hegel.Draw(ht, hegel.SampledFrom([]string{"LOCALHOST", "[::1]", "[FE80::A%25En0]", "SERVICE.example"}))
		space := strings.Join(hegel.Draw(ht, hegel.Lists(hegel.SampledFrom([]string{" ", "\t", "\n", "\u00a0", "\u2003"}))), "")
		authority := host
		if !(scheme == "http" && port == 80 || scheme == "https" && port == 443) {
			authority += fmt.Sprintf(":%d", port)
		}
		want := scheme + "://" + strings.ReplaceAll(strings.ToLower(authority), "en0", "En0") + "/V1%2fItem"
		raw := space + strings.ToUpper(scheme) + "://" + host + fmt.Sprintf(":%d/V1%%2fItem///", port) + space
		got, err := endpointpolicy.CanonicalEndpoint(raw, true, "catalog")
		require.NoError(ht, err)
		assert.Equal(ht, want, got)
	}, hegel.WithDatabase(t.TempDir()))
}

func TestDecodedParentSegments(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		dot := func() string { return hegel.Draw(ht, hegel.SampledFrom([]string{".", "%2e", "%2E"})) }
		sep := func() string { return hegel.Draw(ht, hegel.SampledFrom([]string{"/", "%2f", "%2F"})) }
		path := "/safe" + sep() + dot() + dot() + sep() + "child"
		trust := hegel.Draw(ht, hegel.Booleans())
		_, err := endpointpolicy.CanonicalEndpoint("https://example.test"+path, trust, "catalog")
		require.EqualError(ht, err, "catalog endpoint path must not contain a parent segment")
	}, hegel.WithDatabase(t.TempDir()))
}

func TestStructuralFaultsAndPrecedence(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		path := url.PathEscape(hegel.Draw(ht, hegel.Text()))
		fault := hegel.Draw(ht, hegel.SampledFrom([]struct{ raw, message string }{
			{"ftp://user@example.test/../?q=1", "endpoint must use http or https"},
			{"https://:443/" + path, "endpoint must include a host"},
			{"https://@example.test/" + path + "?q=1", "endpoint must not include userinfo"},
			{"http://example.test/../?q=" + path, "endpoint must not include a query or fragment"},
			{"http://example.test/#value", "endpoint must not include a query or fragment"},
			{"http://example.test/../", "endpoint path must not contain a parent segment"},
		}))
		trust := hegel.Draw(ht, hegel.Booleans())
		got, err := endpointpolicy.CanonicalEndpoint(fault.raw, trust, "")
		require.EqualError(ht, err, fault.message)
		assert.Empty(ht, got)
	}, hegel.WithDatabase(t.TempDir()))
}

func TestSubjectDiagnostics(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		raw := hegel.Draw(ht, hegel.OneOf(hegel.Text(), hegel.URLs()))
		subject := hegel.Draw(ht, hegel.OneOf(hegel.Text(), hegel.Map(hegel.Lists(hegel.Integers[uint8](0, 255)), func(b []byte) string { return string(b) })))
		prefix := subject
		if prefix != "" {
			prefix += " "
		}
		trust := hegel.Draw(ht, hegel.Booleans())
		want, wantErr := endpointpolicy.CanonicalEndpoint(raw, trust, "")
		got, err := endpointpolicy.CanonicalEndpoint(raw, trust, subject)
		assert.Equal(ht, want, got)
		if wantErr == nil {
			require.NoError(ht, err)
		} else {
			require.EqualError(ht, err, prefix+wantErr.Error())
		}
		u := &url.URL{Scheme: hegel.Draw(ht, hegel.Text()), Host: raw}
		want, wantErr = endpointpolicy.Origin(u, "")
		got, err = endpointpolicy.Origin(u, subject)
		assert.Equal(ht, want, got)
		if wantErr == nil {
			require.NoError(ht, err)
		} else {
			require.EqualError(ht, err, prefix+wantErr.Error())
		}
	}, hegel.WithDatabase(t.TempDir()))
}

func TestOriginProjectionAndWrapper(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		scheme := hegel.Draw(ht, hegel.SampledFrom([]string{"https", "HTTPS", "http", "HtTp", "ftp"}))
		host := hegel.Draw(ht, hegel.SampledFrom([]string{"EXAMPLE.TEST", "127.0.0.1", "[FE80::1%En0]"}))
		port := hegel.Draw(ht, hegel.SampledFrom([]string{"80", "443", "8080"}))
		u := &url.URL{
			Scheme: scheme, Host: host + ":" + port,
			Path: hegel.Draw(ht, hegel.Text()), RawPath: hegel.Draw(ht, hegel.Text()),
			RawQuery: hegel.Draw(ht, hegel.Text()), Fragment: hegel.Draw(ht, hegel.Text()),
			Opaque: hegel.Draw(ht, hegel.Text()), ForceQuery: hegel.Draw(ht, hegel.Booleans()),
			User: url.UserPassword(hegel.Draw(ht, hegel.Text()), hegel.Draw(ht, hegel.Text())),
		}
		wantHost := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(host), "%", "%25"), "en0", "En0")
		if !(scheme == "https" && port == "443" || scheme == "http" && port == "80") {
			wantHost += ":" + port
		}
		want := strings.ToLower(scheme) + "://" + wantHost
		got, err := endpointpolicy.Origin(u, "embed")
		require.NoError(ht, err)
		assert.Equal(ht, want, got)
		wrapped, err := embedconfig.Origin(u)
		require.NoError(ht, err)
		assert.Equal(ht, want, wrapped)
	}, hegel.WithDatabase(t.TempDir()))
}
