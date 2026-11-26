package embedmodel_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
)

func TestVertexIdentity(t *testing.T) {
	desc := embedmodel.Descriptor{
		Model:      embedconfig.Model{Name: "model", Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationL2},
		Deployment: embedconfig.Deployment{API: embedconfig.APIVertex},
	}
	vertex, err := desc.VectorIdentity()
	require.NoError(t, err)
	assert.Equal(t, "979bfeb1cf23fa5204468303a16c8017b13edfb49c41cb89915121ae4fed23a5", vertex)

	desc.Model.RequestDimensions = true
	requested, err := desc.VectorIdentity()
	require.NoError(t, err)
	assert.Equal(t, vertex, requested)
	desc.Roles.InputType = embedconfig.InputTypeRetrieval
	retrieval, err := desc.VectorIdentity()
	require.NoError(t, err)
	assert.Equal(t, vertex, retrieval)

	desc.Deployment = embedconfig.Deployment{}
	openAI, err := desc.VectorIdentity()
	require.NoError(t, err)
	desc.Deployment.API = embedconfig.APIOpenAI
	explicitOpenAI, err := desc.VectorIdentity()
	require.NoError(t, err)
	assert.Equal(t, openAI, explicitOpenAI)
	t.Run("kit role protocol", func(t *testing.T) {
		desc := embedmodel.Descriptor{Model: embedconfig.Model{Name: "model", Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Deployment: embedconfig.Deployment{API: embedconfig.APIKit}}
		kit, err := desc.VectorIdentity()
		require.NoError(t, err)
		desc.Model.RequestDimensions = true
		desc.Roles.InputType = embedconfig.InputTypeRetrieval
		same, err := desc.VectorIdentity()
		require.NoError(t, err)
		assert.Equal(t, kit, same)
		desc.Deployment.API = embedconfig.APIOpenAI
		openai, err := desc.VectorIdentity()
		require.NoError(t, err)
		assert.NotEqual(t, kit, openai)
	})
}
