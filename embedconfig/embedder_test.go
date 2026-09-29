package embedconfig_test

import (
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/embedconfig"
)

func TestEmbedderDecodesTheStandardKeys(t *testing.T) {
	var file struct {
		Search struct {
			Embeddings embedconfig.Embedder `toml:"embeddings"`
		} `toml:"search"`
	}
	meta, err := toml.Decode(`
[search.embeddings]
base_url = "https://api.example.test/v1"
model = "embed-large"
dims = 1024
api_key_env = "EMBED_KEY"
fingerprint_salt = "weights-2"
input_type_mode = "retrieval"
batch_size = 16
model_context_tokens = 512
max_batch_tokens = 8192
timeout_seconds = 45
trust_private_network = true
`, &file)
	require.NoError(t, err)
	assert.Empty(t, meta.Undecoded())

	embedder := file.Search.Embeddings
	require.NoError(t, embedder.Validate())
	parts, err := embedder.Parts()
	require.NoError(t, err)
	assert.Equal(t, embedconfig.Model{
		Name: "embed-large", Revision: "weights-2", Dimensions: 1024,
		Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationL2,
	}, parts.Model)
	assert.Equal(t, embedconfig.InputTypeRetrieval, parts.Roles.InputType)
	assert.Equal(t, embedconfig.Deployment{
		BaseURL: "https://api.example.test/v1", TrustPrivateNetwork: true,
	}, parts.Deployment)
	assert.Equal(t, embedconfig.Batch{Items: 16, MaxTokens: 8192, InputTokenUpperBound: 512}, parts.Batch)
	assert.Equal(t, 45*time.Second, parts.Transport.Timeout)
}

func TestEmbedderPartsFillOnlyOperationalDefaults(t *testing.T) {
	parts, err := embedconfig.Embedder{
		BaseURL: "http://127.0.0.1:11434/v1", Model: "nomic", Dims: 768,
	}.Parts()
	require.NoError(t, err)
	assert.Equal(t, embedconfig.DefaultBatchItems, parts.Batch.Items)
	assert.Equal(t, embedconfig.DefaultTimeout, parts.Transport.Timeout)
	assert.Equal(t, embedconfig.InputTypeNone, parts.Roles.InputType)
	assert.False(t, parts.Deployment.PinEndpoint)
}

func TestEmbedderValidate(t *testing.T) {
	valid := embedconfig.Embedder{BaseURL: "https://api.example.test/v1", Model: "m", Dims: 8}
	with := func(change func(*embedconfig.Embedder)) embedconfig.Embedder {
		embedder := valid
		change(&embedder)
		return embedder
	}
	tests := []struct {
		name     string
		embedder embedconfig.Embedder
		valid    bool
	}{
		{name: "disabled", embedder: embedconfig.Embedder{}, valid: true},
		{name: "complete", embedder: valid, valid: true},
		{name: "setting without endpoint", embedder: embedconfig.Embedder{BatchSize: 8}},
		{name: "missing dims", embedder: with(func(e *embedconfig.Embedder) { e.Dims = 0 })},
		{name: "missing model", embedder: with(func(e *embedconfig.Embedder) { e.Model = "" })},
		{name: "both key sources", embedder: with(func(e *embedconfig.Embedder) {
			e.APIKey, e.APIKeyEnv = "secret", "EMBED_KEY"
		})},
		{name: "unknown input type", embedder: with(func(e *embedconfig.Embedder) { e.InputTypeMode = "search" })},
		{name: "negative batch", embedder: with(func(e *embedconfig.Embedder) { e.BatchSize = -1 })},
		{name: "negative timeout", embedder: with(func(e *embedconfig.Embedder) { e.TimeoutSeconds = -1 })},
		{name: "half a token budget", embedder: with(func(e *embedconfig.Embedder) { e.MaxBatchTokens = 8192 })},
		{name: "plaintext public endpoint", embedder: with(func(e *embedconfig.Embedder) {
			e.BaseURL = "http://api.example.test/v1"
		})},
		{name: "plaintext trusted private endpoint", embedder: with(func(e *embedconfig.Embedder) {
			e.BaseURL, e.TrustPrivateNetwork = "http://10.1.2.3:8080/v1", true
		}), valid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.embedder.Validate()
			if test.valid {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
		})
	}
}

func TestEmbedderResolveAPIKey(t *testing.T) {
	key, err := embedconfig.Embedder{APIKey: "inline"}.ResolveAPIKey()
	require.NoError(t, err)
	assert.Equal(t, "inline", key)

	key, err = embedconfig.Embedder{}.ResolveAPIKey()
	require.NoError(t, err)
	assert.Empty(t, key)

	t.Setenv("KIT_TEST_EMBED_KEY", "from-env")
	key, err = embedconfig.Embedder{APIKeyEnv: "KIT_TEST_EMBED_KEY"}.ResolveAPIKey()
	require.NoError(t, err)
	assert.Equal(t, "from-env", key)

	t.Setenv("KIT_TEST_EMBED_KEY", " ")
	_, err = embedconfig.Embedder{APIKeyEnv: "KIT_TEST_EMBED_KEY"}.ResolveAPIKey()
	require.ErrorContains(t, err, "KIT_TEST_EMBED_KEY")

	_, err = embedconfig.Embedder{APIKey: "inline", APIKeyEnv: "KIT_TEST_EMBED_KEY"}.ResolveAPIKey()
	require.Error(t, err)
}
