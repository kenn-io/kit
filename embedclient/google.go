package embedclient

import (
	"fmt"
	"net/http"

	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/auth/httptransport"
	"go.kenn.io/kit/embedconfig"
)

// GoogleHTTPClient returns a client using Google Application Default Credentials.
// Pass it to Options.HTTP to retain origin pinning and automatic token refresh.
func GoogleHTTPClient() (*http.Client, error) {
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"}})
	if err != nil {
		return nil, fmt.Errorf("detect Google credentials: %w", err)
	}
	client, err := httptransport.NewClient(&httptransport.Options{Credentials: creds})
	if err != nil {
		return nil, fmt.Errorf("create Google HTTP client: %w", err)
	}
	client.Timeout = embedconfig.DefaultTimeout
	return client, nil
}
