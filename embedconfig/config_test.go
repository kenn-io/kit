package embedconfig_test

import (
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/embedconfig"
)

func TestPrepareEndpointRetrievalSetup(t *testing.T) {
	got, err := endpointRetrievalSetup().Prepare()
	require.NoError(t, err)

	assert.Equal(t, embedconfig.DefaultBatchItems, got.Batch.Items)
	assert.Equal(t, embedconfig.DefaultTimeout, got.Transport.Timeout)
	assert.Equal(t, embedconfig.DefaultMaxResponseBytes, got.Transport.MaxResponseBytes)
	assert.Equal(t, 1536, got.Model.Dimensions)
	assert.Equal(t, embedconfig.InputTypeRetrieval, got.Roles.InputType)
	assert.Equal(t, embedconfig.ServeListed, got.Serving)
	assert.Equal(t, 1000, got.Retrieval.RawCandidates)
	assert.Equal(t, 20, got.Retrieval.Results)
	assert.True(t, got.Deployment.PinEndpoint)

	identity, err := embedconfig.VectorIdentity(got.Model, got.Roles, got.Deployment)
	require.NoError(t, err)
	moved := got
	moved.Deployment.BaseURL = "https://example.test:443/moved"
	movedIdentity, err := embedconfig.VectorIdentity(moved.Model, moved.Roles, moved.Deployment)
	require.NoError(t, err)
	assert.NotEqual(t, identity, movedIdentity)
}

func TestPreparePrefixedInputSetup(t *testing.T) {
	got, err := prefixedInputSetup().Prepare()
	require.NoError(t, err)

	assert.Equal(t, 32, got.Batch.Items)
	assert.Equal(t, 45*time.Second, got.Transport.Timeout)
	assert.Equal(t, 8192, got.Batch.MaxTokens)
	assert.Equal(t, 512, got.Input.MaxTokens)
	assert.Equal(t, embedconfig.TruncationDropTail, got.Input.Truncation)
	assert.Equal(t, embedconfig.ServeActive, got.Serving)
	assert.False(t, got.Deployment.PinEndpoint)
	assert.Equal(t, "http://127.0.0.1:11434/v1", got.Deployment.BaseURL)

	vectorID, err := embedconfig.VectorIdentity(got.Model, got.Roles, got.Deployment)
	require.NoError(t, err)
	inputID, err := embedconfig.InputIdentity(got.Model, got.Roles, got.Deployment, got.Input)
	require.NoError(t, err)
	assert.NotEqual(t, vectorID, inputID)

	relocated := got
	relocated.Deployment.BaseURL = "http://127.0.0.1:11435/v1"
	relocatedID, err := embedconfig.VectorIdentity(relocated.Model, relocated.Roles, relocated.Deployment)
	require.NoError(t, err)
	assert.Equal(t, vectorID, relocatedID, "unpinned endpoint move keeps the vector identity")
}

func TestOperationalSettingsDoNotChangeIdentity(t *testing.T) {
	setup, err := prefixedInputSetup().Prepare()
	require.NoError(t, err)
	vectorID, err := embedconfig.VectorIdentity(setup.Model, setup.Roles, setup.Deployment)
	require.NoError(t, err)
	inputID, err := embedconfig.InputIdentity(setup.Model, setup.Roles, setup.Deployment, setup.Input)
	require.NoError(t, err)

	changed := setup
	changed.Batch.Items = 128
	changed.Batch.MaxTokens = 16000
	changed.Batch.InputTokenUpperBound = 256
	changed.Transport.Timeout = 45 * time.Second
	changed.Transport.MaxResponseBytes = 1024
	changed.Retrieval.RawCandidates = 10
	changed.Retrieval.Results = 4
	changed.Retrieval.Timeout = time.Second
	changed.Serving = embedconfig.ServeListed
	changed.Deployment.TrustPrivateNetwork = true

	gotVector, err := embedconfig.VectorIdentity(changed.Model, changed.Roles, changed.Deployment)
	require.NoError(t, err)
	gotInput, err := embedconfig.InputIdentity(changed.Model, changed.Roles, changed.Deployment, changed.Input)
	require.NoError(t, err)
	assert.Equal(t, vectorID, gotVector)
	assert.Equal(t, inputID, gotInput)

	changed.Model.EncodingFormat = "base64"
	gotVector, err = embedconfig.VectorIdentity(changed.Model, changed.Roles, changed.Deployment)
	require.NoError(t, err)
	gotInput, err = embedconfig.InputIdentity(changed.Model, changed.Roles, changed.Deployment, changed.Input)
	require.NoError(t, err)
	assert.Equal(t, vectorID, gotVector, "wire encoding is not part of the vector identity")
	assert.Equal(t, inputID, gotInput)
}

func TestInputWindowChangesInputIdentityOnly(t *testing.T) {
	setup, err := prefixedInputSetup().Prepare()
	require.NoError(t, err)
	vectorID, err := embedconfig.VectorIdentity(setup.Model, setup.Roles, setup.Deployment)
	require.NoError(t, err)
	inputID, err := embedconfig.InputIdentity(setup.Model, setup.Roles, setup.Deployment, setup.Input)
	require.NoError(t, err)

	changed := setup.Input
	changed.OverlapTokens = 32
	gotVector, err := embedconfig.VectorIdentity(setup.Model, setup.Roles, setup.Deployment)
	require.NoError(t, err)
	gotInput, err := embedconfig.InputIdentity(setup.Model, setup.Roles, setup.Deployment, changed)
	require.NoError(t, err)
	assert.Equal(t, vectorID, gotVector)
	assert.NotEqual(t, inputID, gotInput)
}

func TestPrepareRejectsIncompleteModelAndBudgets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*embedconfig.Setup)
	}{
		{name: "missing dimensions", mutate: func(s *embedconfig.Setup) { s.Model.Dimensions = 0 }},
		{name: "missing metric", mutate: func(s *embedconfig.Setup) { s.Model.Metric = "" }},
		{name: "negative batch", mutate: func(s *embedconfig.Setup) { s.Batch.Items = -1 }},
		{name: "token bound exceeds cap", mutate: func(s *embedconfig.Setup) {
			s.Batch.MaxTokens = 10
			s.Batch.InputTokenUpperBound = 11
		}},
		{name: "negative timeout", mutate: func(s *embedconfig.Setup) { s.Transport.Timeout = -time.Second }},
		{name: "missing serving", mutate: func(s *embedconfig.Setup) { s.Serving = "" }},
		{name: "raw window below results", mutate: func(s *embedconfig.Setup) {
			s.Retrieval.RawCandidates = 2
			s.Retrieval.Results = 5
		}},
		{name: "partial retrieval", mutate: func(s *embedconfig.Setup) {
			s.Retrieval = embedconfig.Retrieval{Results: 5}
		}},
		{name: "token window without truncation", mutate: func(s *embedconfig.Setup) {
			s.Input.MaxTokens = 128
			s.Input.Tokenizer = "piece"
		}},
		{name: "plaintext public endpoint", mutate: func(s *embedconfig.Setup) {
			s.Deployment.BaseURL = "http://example.test/v1"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setup := endpointRetrievalSetup()
			test.mutate(&setup)
			_, err := setup.Prepare()
			require.Error(t, err)
		})
	}
}

func TestCanonicalEndpoint(t *testing.T) {
	got, err := embedconfig.CanonicalEndpoint("HTTPS://Example.TEST:443/v1/", false)
	require.NoError(t, err)
	assert.Equal(t, "https://example.test/v1", got)

	_, err = embedconfig.CanonicalEndpoint("https://user:secret@example.test/v1", false)
	require.Error(t, err)
	_, err = embedconfig.CanonicalEndpoint("https://example.test/v1?q=1", false)
	require.Error(t, err)

	_, err = embedconfig.CanonicalEndpoint("http://10.1.2.3:8080/v1", false)
	require.Error(t, err)
	trusted, err := embedconfig.CanonicalEndpoint("http://10.1.2.3:8080/v1", true)
	require.NoError(t, err)
	assert.Equal(t, "http://10.1.2.3:8080/v1", trusted)

	cgnat, err := embedconfig.CanonicalEndpoint("http://100.64.0.8/v1", true)
	require.NoError(t, err)
	assert.Equal(t, "http://100.64.0.8/v1", cgnat)

	loopback, err := embedconfig.CanonicalEndpoint("http://127.0.0.1:8080/v1", false)
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:8080/v1", loopback)

	_, err = embedconfig.CanonicalEndpoint("http://gpu-box.local/v1", false)
	require.Error(t, err)
	named, err := embedconfig.CanonicalEndpoint("http://gpu-box.local:11434/v1", true)
	require.NoError(t, err)
	assert.Equal(t, "http://gpu-box.local:11434/v1", named)

	_, err = embedconfig.CanonicalEndpoint("https://example.test/v1/../admin", false)
	require.Error(t, err)
	_, err = embedconfig.CanonicalEndpoint("https://example.test/v1/%2e%2e/admin", false)
	require.Error(t, err)
	dotted, err := embedconfig.CanonicalEndpoint("https://example.test/v1..2/models/", false)
	require.NoError(t, err)
	assert.Equal(t, "https://example.test/v1..2/models", dotted)
}

func TestCanonicalEndpointHostAndIPv6Zone(t *testing.T) {
	_, err := embedconfig.CanonicalEndpoint("https://:443/v1", false)
	require.Error(t, err)
	portOnly, err := url.Parse("https://:443/v1")
	require.NoError(t, err)
	_, err = embedconfig.Origin(portOnly)
	require.Error(t, err)

	plain, err := embedconfig.CanonicalEndpoint("https://[::1]:443/v1/", false)
	require.NoError(t, err)
	assert.Equal(t, "https://[::1]/v1", plain)

	const want = "https://[fe80::1%25Eth0]/v1"
	got, err := embedconfig.CanonicalEndpoint("https://[fe80::1%Eth0]/v1", false)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	encoded, err := embedconfig.CanonicalEndpoint("https://[FE80::1%25Eth0]/v1/", false)
	require.NoError(t, err)
	assert.Equal(t, want, encoded)
	again, err := embedconfig.CanonicalEndpoint(got, false)
	require.NoError(t, err)
	assert.Equal(t, got, again)
	assert.Contains(t, again, "Eth0")

	parsed, err := url.Parse(got)
	require.NoError(t, err)
	origin, err := embedconfig.Origin(parsed)
	require.NoError(t, err)
	assert.Equal(t, "https://[fe80::1%25Eth0]", origin)

	_, err = embedconfig.CanonicalEndpoint("http://[fe80::1%eth0]/v1", false)
	require.Error(t, err)
	trusted, err := embedconfig.CanonicalEndpoint("http://[fe80::1%Eth0]:8080/v1/", true)
	require.NoError(t, err)
	assert.Equal(t, "http://[fe80::1%25Eth0]:8080/v1", trusted)
	trustedAgain, err := embedconfig.CanonicalEndpoint(trusted, true)
	require.NoError(t, err)
	assert.Equal(t, trusted, trustedAgain)
}

func endpointRetrievalSetup() embedconfig.Setup {
	return embedconfig.Setup{
		Model: embedconfig.Model{
			Name:              "text-embedding-3-small",
			Dimensions:        1536,
			Metric:            embedconfig.MetricCosine,
			Normalization:     embedconfig.NormalizationL2,
			RequestDimensions: true,
			EncodingFormat:    "float",
		},
		Roles:      embedconfig.Roles{InputType: embedconfig.InputTypeRetrieval},
		Deployment: embedconfig.Deployment{BaseURL: "https://example.test/v1", PinEndpoint: true},
		Retrieval: embedconfig.Retrieval{
			RawCandidates: 1000,
			Results:       20,
			Timeout:       3 * time.Second,
		},
		Serving: embedconfig.ServeListed,
	}
}

func prefixedInputSetup() embedconfig.Setup {
	return embedconfig.Setup{
		Model: embedconfig.Model{
			Name:          "bge-m3",
			Revision:      "weights-epoch",
			Dimensions:    1024,
			Metric:        embedconfig.MetricCosine,
			Normalization: embedconfig.NormalizationL2,
			Pooling:       "cls",
		},
		Roles: embedconfig.Roles{
			DocumentPrefix: "search_document: ",
			QueryPrefix:    "search_query: ",
		},
		Deployment: embedconfig.Deployment{BaseURL: "http://127.0.0.1:11434/v1"},
		Batch: embedconfig.Batch{
			Items:                32,
			MaxTokens:            8192,
			InputTokenUpperBound: 512,
		},
		Transport: embedconfig.Transport{Timeout: 45 * time.Second},
		Input: embedconfig.InputLimits{
			Recipe:            "v2",
			Tokenizer:         "bge-m3",
			TokenizerRevision: "rev",
			ContentID:         "body",
			MaxTokens:         512,
			OverlapTokens:     64,
			MaxSpans:          50,
			Truncation:        embedconfig.TruncationDropTail,
		},
		Retrieval: embedconfig.Retrieval{RawCandidates: 200, Results: 50},
		Serving:   embedconfig.ServeActive,
	}
}
