package embedconfig_test

import (
	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
	"testing"
)

func TestVertexBaseURL(t *testing.T) {
	got, err := embedconfig.VertexBaseURL("project-a", "us-central1")
	require.NoError(t, err)
	assert.Equal(t, "https://us-central1-aiplatform.googleapis.com/v1/projects/project-a/locations/us-central1/publishers/google/models", got)
	for _, v := range []string{"", "../x", "A", "a/b", "a.b", "-abc", "abc-"} {
		_, err := embedconfig.VertexBaseURL(v, "us-central1")
		assert.Error(t, err)
		_, err = embedconfig.VertexBaseURL("project-a", v)
		assert.Error(t, err)
	}
}

func TestVertexEmbedderAPI(t *testing.T) {
	var e embedconfig.Embedder
	_, err := toml.Decode("api = \"vertex\"\nbase_url = \"https://example.com/models\"\nmodel = \"model\"\ndims = 2\n", &e)
	require.NoError(t, err)
	require.NoError(t, e.Validate())
	parts, err := e.Parts()
	require.NoError(t, err)
	assert.Equal(t, embedconfig.APIVertex, parts.Deployment.API)
	e.API = "invalid"
	assert.Error(t, e.Validate())
}
