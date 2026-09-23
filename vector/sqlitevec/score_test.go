package sqlitevec

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScoreFromDistanceRejectsNonFinite(t *testing.T) {
	score, err := scoreFromDistance(0.25)
	require.NoError(t, err)
	assert.InDelta(t, 0.75, float64(score), 1e-6)

	_, err = scoreFromDistance(math.NaN())
	require.ErrorContains(t, err, "non-finite")

	_, err = scoreFromDistance(math.Inf(1))
	require.ErrorContains(t, err, "non-finite")
}
