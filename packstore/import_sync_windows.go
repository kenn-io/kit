//go:build windows

package packstore

import "os"

// Windows cannot portably fsync directory handles. Pack contents are still
// synced before publication, matching the durability policy used by pack.Writer.
func syncImportRootDir(_ *os.Root, _ string) error { return nil }
