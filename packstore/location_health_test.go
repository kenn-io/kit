package packstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLocationHealthPrefersTransientlyUnavailableOverKnownDamage(t *testing.T) {
	health := NewHealth()
	corrupt := ReadLocation{
		StoreID: "corrupt", Generation: "corrupt-1", Loose: &LooseLocation{},
	}
	unavailable := ReadLocation{
		StoreID: "unavailable", Generation: "unavailable-1", Loose: &LooseLocation{},
	}
	health.Observe(corrupt, ErrPhysicalCorrupt)
	health.Observe(unavailable, ErrStoreUnavailable)

	ordered := health.Order([]ReadLocation{corrupt, unavailable})

	assert.Equal(t, []ReadLocation{unavailable, corrupt}, ordered)
}
