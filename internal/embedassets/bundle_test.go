package embedassets

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWindowsRuntimeFilesMatchArchitecture(t *testing.T) {
	assert.Equal(t, []string{"onnxruntime.dll", "msvcp140.dll", "msvcp140_1.dll", "vcruntime140.dll", "vcruntime140_1.dll"}, nativeFiles("windows", "amd64"))
	// ARM64 ONNX and CRT imports use vcruntime140.dll. The redistribution's
	// vcruntime140_1.dll is AMD64 and cannot be a required ARM64 library.
	assert.Equal(t, []string{"onnxruntime.dll", "msvcp140.dll", "msvcp140_1.dll", "vcruntime140.dll"}, nativeFiles("windows", "arm64"))
}
