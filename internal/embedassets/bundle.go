package embedassets

import "runtime"

// BundleTag selects a published server with the pinned model and runtime.
// Publish all six bundles before changing this tag.
const BundleTag = "v0.48.2-face.6"

// ExecutableName is the bundle's platform executable.
func ExecutableName() string {
	if runtime.GOOS == "windows" {
		return "kennembed.exe"
	}
	return "kennembed"
}

// LibraryName is the runtime loaded beside the server.
func LibraryName() string {
	return libraryName(runtime.GOOS)
}

func libraryName(goos string) string {
	switch goos {
	case "windows":
		return "onnxruntime.dll"
	case "darwin":
		return "libonnxruntime.1.23.2.dylib"
	default:
		return "libonnxruntime.so.1.23.2"
	}
}

// NativeFiles lists required libraries, including Windows runtime dependencies.
func NativeFiles() []string {
	return nativeFiles(runtime.GOOS, runtime.GOARCH)
}

func nativeFiles(goos, goarch string) []string {
	names := []string{libraryName(goos)}
	if goos == "windows" {
		names = append(names, "msvcp140.dll", "msvcp140_1.dll", "vcruntime140.dll")
		// ARM64 ONNX and CRTs do not import this AMD64-only runtime.
		if goarch == "amd64" {
			names = append(names, "vcruntime140_1.dll")
		}
	}
	return names
}
