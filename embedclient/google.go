package embedclient

import (
	"fmt"
	"net/http"

	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/auth/httptransport"
)

// GoogleHTTPClient returns a client using Google Application Default Credentials.
// Pass it to Options.HTTP to retain origin pinning and automatic token refresh.
// Its Google credentials take precedence over Options.APIKey. The returned
// client has no timeout; New applies Transport.Timeout for Vertex requests.
func GoogleHTTPClient() (*http.Client, error) {
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"}})
	if err != nil {
		return nil, fmt.Errorf("detect Google credentials: %w", err)
	}
	client, err := httptransport.NewClient(&httptransport.Options{Credentials: creds})
	if err != nil {
		return nil, fmt.Errorf("create Google HTTP client: %w", err)
	}
	return client, nil
}
