package s3store

import (
	"context"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

func TestNewNormalizesDefaultLimits(t *testing.T) {
	backend, err := New(context.Background(), testConfig())

	require.NoError(t, err)
	assert.Equal(t, packstore.DefaultLimits(), backend.limits)
}

func TestNewRejectsPartialInvalidLimits(t *testing.T) {
	config := testConfig()
	config.Limits = packstore.Limits{
		BlobBytes: 1, PackBytes: 1, FooterBytes: 1,
	}

	_, err := New(context.Background(), config)

	require.ErrorContains(t, err, "invalid")
}

func testConfig() Config {
	return Config{
		Region: "us-east-1",
		Bucket: "test-bucket",
		Credentials: credentials.NewStaticCredentialsProvider(
			"test-access-key",
			"test-secret-key",
			"",
		),
	}
}

func newHTTPBackend(
	limits packstore.Limits,
	roundTrip func(*http.Request) (*http.Response, error),
) *Backend {
	client := s3.NewFromConfig(aws.Config{
		Region: "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(
			"test-access-key",
			"test-secret-key",
			"",
		),
		HTTPClient: &http.Client{Transport: roundTripFunc(roundTrip)},
	}, func(options *s3.Options) {
		options.BaseEndpoint = aws.String("https://example.test")
		options.UsePathStyle = true
	})
	return &Backend{
		client: client,
		bucket: "test-bucket",
		part:   5,
		page:   defaultInventorySize,
		limits: limits,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
