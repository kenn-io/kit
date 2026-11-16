//go:build !unix

package selfupdate

func stageBinary(srcPath, dstDir, dstBase string) (string, error) {
	return writeStagedBinary(srcPath, dstDir, dstBase)
}
