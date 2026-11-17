package embedserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIntraOpThreads(t *testing.T) {
	for _, tc := range []struct{ gomaxprocs, quota, physical, want int }{
		{256, 8, 256, 8}, {8, 0, 256, 8}, {2, 8, 256, 2},
		{256, 0, 256, 0}, {256, 300, 256, 0}, {16, 12, 8, 0},
		{8, 0, 8, 0}, {16, 0, 8, 0}, {1, 0, 2, 1},
	} {
		assert.Equal(t, tc.want, intraOpThreads(tc.gomaxprocs, tc.quota, tc.physical), "%+v", tc)
	}
}
