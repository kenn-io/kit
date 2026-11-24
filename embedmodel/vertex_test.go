package embedmodel_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
	"testing"
)

func TestVertexIdentity(t *testing.T) {
	d := sampleDescriptor()
	original, err := d.VectorIdentity()
	require.NoError(t, err)
	d.Deployment.API = embedconfig.APIOpenAI
	explicit, err := d.VectorIdentity()
	require.NoError(t, err)
	assert.Equal(t, original, explicit)
	d.Deployment.API = embedconfig.APIVertex
	vertex, err := d.VectorIdentity()
	require.NoError(t, err)
	assert.NotEqual(t, original, vertex)
	d.Model.RequestDimensions = true
	requested, err := d.VectorIdentity()
	require.NoError(t, err)
	assert.Equal(t, vertex, requested)
}
