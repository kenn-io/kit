package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeStoreWriteCheckStaysOutsideDiscoveryNamespaces(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := RuntimeStore{Dir: t.TempDir(), Prefix: "tool"}
	prefix, err := store.validatePrefix()
	require.NoError(err)
	probe, err := os.CreateTemp(store.Dir, fmt.Sprintf(runtimeWriteCheckPattern, prefix))
	require.NoError(err)
	t.Cleanup(func() {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
	})

	records, err := store.List()
	require.NoError(err)
	assert.Empty(records)
	name := filepath.Base(probe.Name())
	_, isRecord := pidFromName(prefix, name)
	assert.False(isRecord)
	assert.NotEqual(prefix+".lock", name)
	assert.NotEqual(prefix+".listen.lock", name)
}
