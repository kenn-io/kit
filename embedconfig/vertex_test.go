package embedconfig_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/embedconfig"
)

func TestVertexBaseURL(t *testing.T) {
	got, err := embedconfig.VertexBaseURL("project-a", "us-central1")
	require.NoError(t, err)
	assert.Equal(t, "https://us-central1-aiplatform.googleapis.com/v1/projects/project-a/locations/us-central1/publishers/google/models", got)
	got, err = embedconfig.VertexBaseURL("project-a", "global")
	require.NoError(t, err)
	assert.Equal(t, "https://aiplatform.googleapis.com/v1/projects/project-a/locations/global/publishers/google/models", got)
	for _, location := range []string{"us", "eu"} {
		got, err = embedconfig.VertexBaseURL("project-a", location)
		require.NoError(t, err)
		assert.Equal(t, "https://aiplatform."+location+".rep.googleapis.com/v1/projects/project-a/locations/"+location+"/publishers/google/models", got)
	}
	for _, v := range []string{"", "A", "a/b", "a.b", "-abc", "abc-"} {
		_, err := embedconfig.VertexBaseURL(v, "us-central1")
		require.Error(t, err)
		_, err = embedconfig.VertexBaseURL("project-a", v)
		require.Error(t, err)
	}
}

func TestVertexDeploymentURL(t *testing.T) {
	for _, base := range []string{"https://example.com", "https://example.com/publishers/google/models/model"} {
		err := (embedconfig.Deployment{API: embedconfig.APIVertex, BaseURL: base}).Validate()
		require.ErrorContains(t, err, "VertexBaseURL")
	}
	for _, base := range []string{"https://example.com/v1/projects/p/locations/r/publishers/google/models", "https://example.com/publishers/google/models/"} {
		require.NoError(t, (embedconfig.Deployment{API: embedconfig.APIVertex, BaseURL: base}).Validate())
	}
	require.Error(t, (embedconfig.Deployment{API: "invalid", BaseURL: "https://example.com/v1"}).Validate())
}

func TestKitDeployment(t *testing.T) {
	assert.NoError(t, (embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: "https://example.com/v1"}).Validate())
	assert.Error(t, (embedconfig.Deployment{API: embedconfig.APIKit}).Validate())
}
