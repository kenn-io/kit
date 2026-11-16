//go:build unix

package selfupdate

import "syscall"

func stageBinary(srcPath, dstDir, dstBase string) (string, error) {
	// Close-on-exec still lets a child inherit writable files until exec.
	// Prevent forks throughout staging so verification cannot race a child's
	// inherited writer and fail with ETXTBSY after we close our own file.
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	return writeStagedBinary(srcPath, dstDir, dstBase)
}
