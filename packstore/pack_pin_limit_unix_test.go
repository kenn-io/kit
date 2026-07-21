//go:build unix

package packstore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const sourcePinLimitChild = "KIT_PACKSTORE_SOURCE_PIN_LIMIT_CHILD"

func TestPackSourcePinLimitHonorsUnixSoftLimit(t *testing.T) {
	if os.Getenv(sourcePinLimitChild) == "1" {
		runPackSourcePinLimitChild(t)
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestPackSourcePinLimitHonorsUnixSoftLimit$")
	command.Env = append(os.Environ(), sourcePinLimitChild+"=1")
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
}

func runPackSourcePinLimitChild(t *testing.T) {
	var processLimit unix.Rlimit
	require.NoError(t, unix.Getrlimit(unix.RLIMIT_NOFILE, &processLimit))
	if processLimit.Cur < 160 {
		t.Skip("process soft file limit is already below the controlled fixture")
	}
	processLimit.Cur = 160
	require.NoError(t, unix.Setrlimit(unix.RLIMIT_NOFILE, &processLimit))

	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	var order []Hash
	for index := range 40 {
		content := fmt.Appendf(nil, "soft-limit source %02d", index)
		hash := writeMaintenanceLoose(t, layout, content)
		catalog.addLoose(hash, layout.LoosePath(hash))
		order = append(order, hash)
	}
	catalog.setCandidateOrder(order)
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	assert.Equal(t, 16, maintainer.packedSourcePinLimit)

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.NoError(t, err)
	assert.Equal(t, 40, stats.BlobsPacked)
	assert.Equal(t, 3, stats.PacksSealed)
}
