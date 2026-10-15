package gcs

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Independently validate the wire request using Google's documented canonical
// path and AWS4 interoperability scope, including percent-encoded object names.
func TestXMLSigning(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			observed := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authorization := r.Header.Get("Authorization")
				fields := strings.Split(strings.TrimPrefix(authorization, "AWS4-HMAC-SHA256 "), ", ")
				if len(fields) != 3 {
					observed <- "missing authorization"
					w.WriteHeader(http.StatusForbidden)
					return
				}
				credential := strings.TrimPrefix(fields[0], "Credential=")
				scope := strings.TrimPrefix(credential, "test-key-id/")
				signedHeaders := strings.TrimPrefix(fields[1], "SignedHeaders=")
				var canonicalHeaders strings.Builder
				for name := range strings.SplitSeq(signedHeaders, ";") {
					value := r.Header.Get(name)
					if name == "host" {
						value = r.Host
					}
					if name == "content-length" {
						value = strconv.FormatInt(r.ContentLength, 10)
					}
					canonicalHeaders.WriteString(name + ":" + value + "\n")
				}
				payload := r.Header.Get("x-amz-content-sha256")
				if method == http.MethodPost && payload != "UNSIGNED-PAYLOAD" {
					observed <- "resumable initiation must use unsigned payload"
					w.WriteHeader(http.StatusForbidden)
					return
				}
				canonical := strings.Join([]string{r.Method, r.URL.EscapedPath(), strings.ReplaceAll(r.URL.Query().Encode(), "+", "%20"), canonicalHeaders.String(), signedHeaders, payload}, "\n")
				sum := sha256.Sum256([]byte(canonical))
				toSign := "AWS4-HMAC-SHA256\n" + r.Header.Get("x-amz-date") + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
				key := []byte("AWS4test-secret")
				for part := range strings.SplitSeq(scope, "/") {
					mac := hmac.New(sha256.New, key)
					_, _ = mac.Write([]byte(part))
					key = mac.Sum(nil)
				}
				mac := hmac.New(sha256.New, key)
				_, _ = mac.Write([]byte(toSign))
				signature := hex.EncodeToString(mac.Sum(nil))
				if signature != strings.TrimPrefix(fields[2], "Signature=") {
					observed <- "canonical request mismatch"
					w.WriteHeader(http.StatusForbidden)
					return
				}
				observed <- r.URL.EscapedPath()
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			s, err := New(Config{Bucket: "bucket-a", KeyID: "test-key-id", Secret: "test-secret", Endpoint: server.URL, Client: server.Client()})
			require.NoError(t, err)
			u := s.objectURL("special ?=#!$&'()*+,:;@[]\"%.bin")
			query := u.Query()
			query.Set("marker", "a+b c")
			u.RawQuery = query.Encode()
			var body io.Reader
			var length int64
			if method == http.MethodPut {
				body = bytes.NewReader([]byte("data"))
				length = 4
			}
			response, _, err := s.request(t.Context(), method, u, body, length, http.Header{"x-goog-if-generation-match": {"0"}})
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			assert.Equal(t, 200, response.StatusCode)
			assert.Equal(t, "/bucket-a/special%20%3F%3D%23%21%24%26%27%28%29%2A%2B%2C%3A%3B%40%5B%5D%22%25.bin", <-observed)
		})
	}
}
