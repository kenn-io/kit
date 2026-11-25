package embedmodel

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
)

func TestVertexIdentity(t *testing.T) {
	model := embedconfig.Model{Name: "model", Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationL2}
	deployment := embedconfig.Deployment{API: embedconfig.APIVertex}
	vertex, err := vectorFields(model, embedconfig.Roles{}, deployment)
	require.NoError(t, err)
	openAI, err := vectorFields(model, embedconfig.Roles{}, embedconfig.Deployment{})
	require.NoError(t, err)
	assert.NotEqual(t, openAI, vertex)

	model.RequestDimensions = true
	requested, err := vectorFields(model, embedconfig.Roles{}, deployment)
	require.NoError(t, err)
	assert.Equal(t, vertex, requested)
	retrieval, err := vectorFields(model, embedconfig.Roles{InputType: embedconfig.InputTypeRetrieval}, deployment)
	require.NoError(t, err)
	assert.Equal(t, vertex, retrieval)
}
