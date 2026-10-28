//go:build linux || darwin

package embedserver

import (
	"unsafe"

	"github.com/ebitengine/purego"
	"go.kenn.io/kit/internal/embedassets"
)

func openLibrary(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
}
func closeLibrary(h uintptr) error                          { return purego.Dlclose(h) }
func librarySymbol(h uintptr, name string) (uintptr, error) { return purego.Dlsym(h, name) }
func libraryName() string                                   { return embedassets.LibraryName() }

func modelPath(path string) (unsafe.Pointer, error) {
	b := append([]byte(path), 0)
	return unsafe.Pointer(&b[0]), nil
}
