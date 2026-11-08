package releasecheck

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

type testMember struct {
	name string
	data []byte
	mode int64
	typ  byte
}

func fixture(t *testing.T, targetOS, arch string) []byte {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o600))
	out := filepath.Join(dir, "app")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", out, src)
	cmd.Env = append(os.Environ(), "GOOS="+targetOS, "GOARCH="+arch, "CGO_ENABLED=0")
	log, err := cmd.CombinedOutput()
	require.NoError(t, err, string(log))
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	return data
}

func testArtifact(osName string) Artifact {
	ext := ".tar.gz"
	exe := "app"
	if osName == "windows" {
		ext = ".zip"
		exe = "app.exe"
	}
	return Artifact{Name: "app_{version}_" + osName + "_amd64" + ext, OS: osName, Arch: "amd64", Executable: exe, Kind: "single", Baseline: "reviewed OS baseline", SystemLibraries: map[string][]string{"linux": nil, "darwin": {"/usr/lib/libSystem.B.dylib"}, "windows": {"kernel32.dll"}}[osName], RequiredFiles: []string{"NOTICE"}}
}

func archiveFixture(t *testing.T, a Artifact, members []testMember) (string, string) {
	t.Helper()
	dir := t.TempDir()
	name := strings.ReplaceAll(a.Name, "{version}", "1.2.3")
	var buf bytes.Buffer
	if a.OS == "windows" {
		w := zip.NewWriter(&buf)
		for _, m := range members {
			h := &zip.FileHeader{Name: m.name, Method: zip.Deflate}
			mode := os.FileMode(m.mode)
			if m.typ == tar.TypeSymlink {
				mode |= os.ModeSymlink
			}
			if m.typ == tar.TypeDir {
				mode |= os.ModeDir
			}
			h.SetMode(mode)
			f, e := w.CreateHeader(h)
			require.NoError(t, e)
			_, e = f.Write(m.data)
			require.NoError(t, e)
		}
		require.NoError(t, w.Close())
	} else {
		gz := gzip.NewWriter(&buf)
		w := tar.NewWriter(gz)
		for _, m := range members {
			typ := m.typ
			if typ == 0 {
				typ = tar.TypeReg
			}
			h := &tar.Header{Name: m.name, Mode: m.mode, Size: int64(len(m.data)), Typeflag: typ}
			if typ != tar.TypeReg {
				h.Size = 0
			}
			require.NoError(t, w.WriteHeader(h))
			if typ == tar.TypeReg {
				_, e := w.Write(m.data)
				require.NoError(t, e)
			}
		}
		require.NoError(t, w.Close())
		require.NoError(t, gz.Close())
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600))
	sums := fmt.Sprintf("%x  ./%s\n", sha256.Sum256(buf.Bytes()), name)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums), 0o600))
	return dir, name
}

func TestArchiveContract(t *testing.T) {
	for _, targetOS := range []string{"linux", "darwin", "windows"} {
		t.Run(targetOS, func(t *testing.T) {
			data := fixture(t, targetOS, "amd64")
			a := testArtifact(targetOS)
			good := []testMember{{a.Executable, data, 0o755, 0}, {"NOTICE", []byte("notice"), 0o644, 0}}
			t.Run("valid", func(t *testing.T) {
				dir, _ := archiveFixture(t, a, good)
				require.NoError(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "v1.2.3", ""))
			})
			cases := []struct {
				name    string
				members []testMember
				mutate  func(string, string)
			}{
				{"missing required", good[:1], nil},
				{"empty required", []testMember{good[0], {"NOTICE", nil, 0o644, 0}}, nil},
				{"empty executable", []testMember{{a.Executable, nil, 0o755, 0}, good[1]}, nil},
				{"script", []testMember{{a.Executable, []byte("#!/bin/sh\n"), 0o755, 0}, good[1]}, nil},
				{"duplicate", append(append([]testMember{}, good...), good[0]), nil},
				{"alias", append(append([]testMember{}, good...), testMember{"./" + a.Executable, data, 0o755, 0}), nil},
				{"traversal", append(append([]testMember{}, good...), testMember{"x/../evil", []byte("x"), 0o644, 0}), nil},
				{"symlink", append(append([]testMember{}, good...), testMember{"link", nil, 0o777, tar.TypeSymlink}), nil},
				{"undeclared native", append(append([]testMember{}, good...), testMember{"extra", data, 0o755, 0}), nil},
				{"missing archive", good, func(dir, name string) { require.NoError(t, os.Remove(filepath.Join(dir, name))) }},
				{"missing checksum", good, func(dir, _ string) { require.NoError(t, os.Remove(filepath.Join(dir, "SHA256SUMS"))) }},
				{"changed hash", good, func(dir, name string) {
					require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("changed"), 0o600))
				}},
				{"duplicate checksum", good, func(dir, _ string) {
					p := filepath.Join(dir, "SHA256SUMS")
					b, e := os.ReadFile(p)
					require.NoError(t, e)
					require.NoError(t, os.WriteFile(p, append(b, b...), 0o600))
				}},
			}
			if targetOS != "windows" {
				cases = append(cases, struct {
					name    string
					members []testMember
					mutate  func(string, string)
				}{"non executable mode", []testMember{{a.Executable, data, 0o644, 0}, good[1]}, nil})
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					dir, name := archiveFixture(t, a, tc.members)
					if tc.mutate != nil {
						tc.mutate(dir, name)
					}
					require.Error(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
				})
			}
			t.Run("wrong arch", func(t *testing.T) {
				dir, _ := archiveFixture(t, a, good)
				wrong := a
				wrong.Arch = "arm64"
				require.Error(t, Check(t.Context(), Contract{[]Artifact{wrong}}, dir, "1.2.3", ""))
			})
			t.Run("harmless prefix", func(t *testing.T) {
				members := []testMember{{"./", nil, 0o755, tar.TypeDir}, {"./" + a.Executable, data, 0o755, 0}, good[1]}
				dir, _ := archiveFixture(t, a, members)
				require.NoError(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
			})
		})
	}
}

func TestArchiveRejectsEmptyAndAbsoluteRootDirectories(t *testing.T) {
	a := testArtifact("windows")
	data := fixture(t, "windows", "amd64")
	for _, name := range []string{"", "/"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			members := []testMember{
				{name: name, mode: 0o755, typ: tar.TypeDir},
				{name: a.Executable, data: data, mode: 0o755},
				{name: "NOTICE", data: []byte("notice"), mode: 0o644},
			}
			dir, _ := archiveFixture(t, a, members)
			require.ErrorContains(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""), "unsafe archive path")
		})
	}
	t.Run("absolute tar root", func(t *testing.T) {
		a := testArtifact("linux")
		members := []testMember{
			{name: "/", mode: 0o755, typ: tar.TypeDir},
			{name: a.Executable, data: fixture(t, "linux", "amd64"), mode: 0o755},
			{name: "NOTICE", data: []byte("notice"), mode: 0o644},
		}
		dir, _ := archiveFixture(t, a, members)
		require.ErrorContains(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""), "unsafe archive path")
	})
}

func TestInventoryAndStrictContract(t *testing.T) {
	a := testArtifact("linux")
	b := testArtifact("windows")
	raw, err := json.Marshal(Contract{[]Artifact{a}})
	require.NoError(t, err)
	unknownField := strings.TrimSuffix(string(raw), "}") + `,"unknown":true}`
	trailingJSON := string(raw) + ` {}`
	for _, tc := range []struct {
		input string
		want  string
	}{
		{input: unknownField, want: "unknown field"},
		{input: trailingJSON, want: "trailing JSON"},
		{input: `null`, want: "contract has no artifacts"},
		{input: `{"artifacts":[]}`, want: "contract has no artifacts"},
	} {
		_, err := ReadContract(strings.NewReader(tc.input))
		require.ErrorContains(t, err, tc.want, tc.input)
	}
	got, err := ReadContract(bytes.NewReader(raw))
	require.NoError(t, err)
	assert.Equal(t, a, got.Artifacts[0])
	data := fixture(t, "linux", "amd64")
	dir, _ := archiveFixture(t, a, []testMember{{a.Executable, data, 0o755, 0}, {"NOTICE", []byte("x"), 0o644, 0}})
	c := Contract{[]Artifact{a, b}}
	require.NoError(t, Check(t.Context(), c, dir, "1.2.3", "linux"))
	require.Error(t, Check(t.Context(), c, dir, "1.2.3", ""))
	require.Error(t, Check(t.Context(), c, dir, "1.2.3", "freebsd"))
	c.Artifacts[1].Kind = "invalid"
	require.Error(t, Check(t.Context(), c, dir, "1.2.3", "linux"))
	for _, v := range []string{"", "../1", "a/b", "v", "a\\b"} {
		require.Error(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, v, ""))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, Check(ctx, Contract{[]Artifact{a}}, dir, "1.2.3", ""), context.Canceled)
}

// A changed archive checksum must be rejected independently of which digest
// byte/bit is mutated and of the tar/zip container format.
func TestChecksumMutationProperty(t *testing.T) {
	for _, targetOS := range []string{"linux", "windows"} {
		t.Run(targetOS, func(t *testing.T) {
			a := testArtifact(targetOS)
			data := fixture(t, targetOS, "amd64")
			dir, name := archiveFixture(t, a, []testMember{{a.Executable, data, 0o755, 0}, {"NOTICE", []byte("x"), 0o644, 0}})
			archive, err := os.ReadFile(filepath.Join(dir, name))
			require.NoError(t, err)
			sum := sha256.Sum256(archive)
			require.NoError(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
			rapid.Check(t, func(ht *rapid.T) {
				n := rapid.Uint64().Draw(ht, "mutation")
				changed := sum
				changed[(n/8)%32] ^= byte(1 << (n % 8))
				require.NoError(ht, os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(fmt.Sprintf("%x *%s\n", changed, name)), 0o600))
				require.Error(ht, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
			})
		})
	}
}

func TestPEImportDescriptors(t *testing.T) {
	// Import descriptors name DLLs even when every imported symbol is ordinal.
	for _, ordinal := range []bool{false, true} {
		t.Run(strconv.FormatBool(ordinal), func(t *testing.T) {
			data := syntheticPE(ordinal)
			info, err := inspectNative(data)
			require.NoError(t, err)
			assert.Equal(t, []string{"private.dll"}, info.imports)
			binary.LittleEndian.PutUint32(data[0x200+12:], 0xfffffff0)
			_, err = inspectNative(data)
			require.Error(t, err)
		})
	}
}

func syntheticPE(ordinal bool) []byte {
	b := make([]byte, 1024)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[0x3c:], 0x80)
	copy(b[0x80:], "PE\x00\x00")
	coff := b[0x84:]
	binary.LittleEndian.PutUint16(coff, 0x8664)
	binary.LittleEndian.PutUint16(coff[2:], 1)
	binary.LittleEndian.PutUint16(coff[16:], 240)
	binary.LittleEndian.PutUint16(coff[18:], 0x22)
	opt := coff[20:]
	binary.LittleEndian.PutUint16(opt, 0x20b)
	binary.LittleEndian.PutUint16(opt[68:], 3)
	binary.LittleEndian.PutUint32(opt[16:], 0x1100)
	binary.LittleEndian.PutUint32(opt[108:], 16)
	binary.LittleEndian.PutUint32(opt[120:], 0x1000)
	binary.LittleEndian.PutUint32(opt[124:], 40)
	sec := opt[240:]
	copy(sec, ".idata")
	binary.LittleEndian.PutUint32(sec[8:], 512)
	binary.LittleEndian.PutUint32(sec[12:], 0x1000)
	binary.LittleEndian.PutUint32(sec[16:], 512)
	binary.LittleEndian.PutUint32(sec[20:], 512)
	desc := b[512:]
	binary.LittleEndian.PutUint32(desc, 0x1080)
	binary.LittleEndian.PutUint32(desc[12:], 0x1060)
	binary.LittleEndian.PutUint32(desc[16:], 0x1080)
	copy(b[0x260:], "private.dll\x00")
	if ordinal {
		binary.LittleEndian.PutUint64(b[0x280:], 1<<63|7)
	} else {
		binary.LittleEndian.PutUint64(b[0x280:], 0x10a0)
		copy(b[0x2a2:], "function\x00")
	}
	return b
}

func TestNativeTargetMatrix(t *testing.T) {
	for _, targetOS := range []string{"linux", "darwin", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			t.Run(targetOS+"/"+arch, func(t *testing.T) {
				info, err := inspectNative(fixture(t, targetOS, arch))
				require.NoError(t, err)
				assert.Equal(t, targetOS, info.os)
				assert.Equal(t, arch, info.arch)
				assert.True(t, info.executable)
			})
		}
	}
}

func TestStaticImagePolicy(t *testing.T) {
	data := fixture(t, "linux", "amd64")
	a := testArtifact("linux")
	a.Kind = "static"
	good := []testMember{{a.Executable, data, 0o755, 0}, {"NOTICE", []byte("x"), 0o644, 0}}
	dir, _ := archiveFixture(t, a, good)
	require.NoError(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
	// ET_DYN alone is a shared library, not an executable. An entry point alone
	// must not turn it into a PIE without the ELF interpreter or PIE flag.
	shared := bytes.Clone(data)
	binary.LittleEndian.PutUint16(shared[16:], 3)
	good[0].data = shared
	dir, _ = archiveFixture(t, a, good)
	require.ErrorContains(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""), "not an executable")
}

func TestBundleImportClosure(t *testing.T) {
	a := testArtifact("windows")
	a.SystemLibraries = nil
	a.RequiredFiles = nil
	app := syntheticPE(true)
	dll := syntheticPE(false)
	binary.LittleEndian.PutUint16(dll[0x84+18:], 0x2022)
	clear(dll[0x260:0x280])
	copy(dll[0x260:], "system.dll\x00")
	members := []testMember{{a.Executable, app, 0o755, 0}, {"private.dll", dll, 0o644, 0}}
	t.Run("single rejects private import", func(t *testing.T) {
		dir, _ := archiveFixture(t, a, members[:1])
		require.ErrorContains(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""), "private.dll")
	})
	a.Kind = "bundle"
	a.RuntimeFiles = []string{"private.dll"}
	t.Run("transitive missing", func(t *testing.T) {
		dir, _ := archiveFixture(t, a, members)
		require.ErrorContains(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""), "system.dll")
	})
	a.SystemLibraries = []string{"SYSTEM.DLL"}
	t.Run("complete", func(t *testing.T) {
		dir, _ := archiveFixture(t, a, members)
		require.NoError(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
	})
	t.Run("DLL is not executable", func(t *testing.T) {
		wrong := append([]testMember{}, members...)
		wrong[0].data = dll
		dir, _ := archiveFixture(t, a, wrong)
		require.ErrorContains(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""), "not an executable")
	})
	t.Run("empty runtime", func(t *testing.T) {
		wrong := append([]testMember{}, members...)
		wrong[1].data = nil
		dir, _ := archiveFixture(t, a, wrong)
		require.Error(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
	})
	t.Run("runtime wrong arch", func(t *testing.T) {
		wrong := append([]testMember{}, members...)
		wrong[1].data = bytes.Clone(dll)
		binary.LittleEndian.PutUint16(wrong[1].data[0x84:], 0xaa64)
		dir, _ := archiveFixture(t, a, wrong)
		require.ErrorContains(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""), "target")
	})
	t.Run("absolute import cannot use bundle basename", func(t *testing.T) {
		wrong := append([]testMember{}, members...)
		wrong[0].data = bytes.Clone(app)
		clear(wrong[0].data[0x260:0x280])
		copy(wrong[0].data[0x260:], "C:/external/private.dll\x00")
		dir, _ := archiveFixture(t, a, wrong)
		require.ErrorContains(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""), "unresolved import")
	})
}

func TestContractDeclarationRejections(t *testing.T) {
	cases := map[string]func(*Artifact){
		"archive traversal":   func(a *Artifact) { a.Name = "../app.tar.gz" },
		"unknown placeholder": func(a *Artifact) { a.Name = "app_{bad}.tar.gz" },
		"archive extension":   func(a *Artifact) { a.Name = "app.zip" },
		"invalid os":          func(a *Artifact) { a.OS = "freebsd" },
		"invalid arch":        func(a *Artifact) { a.Arch = "386" },
		"no baseline":         func(a *Artifact) { a.Baseline = " " },
		"exe traversal":       func(a *Artifact) { a.Executable = "a/../app" },
		"backslash":           func(a *Artifact) { a.Executable = "a\\app" },
		"single runtime":      func(a *Artifact) { a.RuntimeFiles = []string{"lib.so"} },
		"bundle empty":        func(a *Artifact) { a.Kind = "bundle" },
		"static non ELF":      func(a *Artifact) { a.OS = "darwin"; a.Kind = "static" },
		"wildcard":            func(a *Artifact) { a.SystemLibraries = []string{"lib*"} },
		"duplicate required":  func(a *Artifact) { a.RequiredFiles = []string{a.Executable} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a := testArtifact("linux")
			mutate(&a)
			raw, err := json.Marshal(Contract{[]Artifact{a}})
			require.NoError(t, err)
			_, err = ReadContract(bytes.NewReader(raw))
			require.Error(t, err)
		})
	}
	a := testArtifact("linux")
	raw, err := json.Marshal(Contract{[]Artifact{a, a}})
	require.NoError(t, err)
	_, err = ReadContract(bytes.NewReader(raw))
	require.Error(t, err)
}

func TestAncillaryChecksums(t *testing.T) {
	a := testArtifact("linux")
	dir, name := archiveFixture(t, a, []testMember{{a.Executable, fixture(t, "linux", "amd64"), 0o755, 0}, {"NOTICE", []byte("x"), 0o644, 0}})
	p := filepath.Join(dir, "SHA256SUMS")
	sums, err := os.ReadFile(p)
	require.NoError(t, err)
	sums = append(sums, []byte(fmt.Sprintf("%x *source.tar.gz\n", sha256.Sum256([]byte("source"))))...)
	require.NoError(t, os.WriteFile(p, sums, 0o600))
	require.NoError(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
	for _, line := range []string{"bad  " + name + "\n", strings.Repeat("z", 64) + "  " + name + "\n", strings.Repeat("0", 64) + "x " + name + "\n"} {
		require.NoError(t, os.WriteFile(p, []byte(line), 0o600))
		require.Error(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
	}
}

func syntheticELF(importName string) []byte {
	b := make([]byte, 512)
	copy(b, "\x7fELF")
	b[4] = 2
	b[5] = 1
	b[6] = 1
	put16 := binary.LittleEndian.PutUint16
	put32 := binary.LittleEndian.PutUint32
	put64 := binary.LittleEndian.PutUint64
	put16(b[16:], 2)
	put16(b[18:], 62)
	put32(b[20:], 1)
	put64(b[24:], 0x400080)
	put64(b[32:], 64)
	put16(b[52:], 64)
	put16(b[54:], 56)
	put16(b[56:], 2)
	p := b[64:]
	put32(p, 1)
	put32(p[4:], 5)
	put64(p[16:], 0x400000)
	put64(p[32:], 512)
	put64(p[40:], 512)
	p = b[120:]
	put32(p, 2)
	put64(p[8:], 256)
	put64(p[16:], 0x400100)
	put64(p[32:], 64)
	put64(p[40:], 64)
	d := b[256:]
	put64(d, 1)
	put64(d[8:], 1)
	put64(d[16:], 5)
	put64(d[24:], 0x400180)
	put64(d[32:], 10)
	put64(d[40:], 64)
	copy(b[385:], importName+"\x00")
	return b
}

func TestSectionlessELFImports(t *testing.T) {
	data := syntheticELF("private.so")
	info, err := inspectNative(data)
	require.NoError(t, err)
	assert.Equal(t, []string{"private.so"}, info.imports)
	a := testArtifact("linux")
	a.RequiredFiles = nil
	a.Kind = "static"
	a.SystemLibraries = []string{"private.so"}
	dir, _ := archiveFixture(t, a, []testMember{{a.Executable, data, 0o755, 0}})
	require.Error(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
	binary.LittleEndian.PutUint64(data[256+8:], 1024)
	_, err = inspectNative(data)
	require.Error(t, err)
}

func TestMachPlatformAndImportCommands(t *testing.T) {
	// LC_LOAD_WEAK_DYLIB must count even though debug/macho does not expose it as
	// a Dylib. LC_BUILD_VERSION distinguishes macOS from iOS on identical CPUs.
	b := make([]byte, 96)
	put := binary.LittleEndian.PutUint32
	put(b, 0xfeedfacf)
	put(b[4:], 0x100000c)
	put(b[8:], 0)
	put(b[12:], 2)
	put(b[16:], 2)
	put(b[20:], 64)
	put(b[32:], 0x32)
	put(b[36:], 24)
	put(b[40:], 1)
	put(b[56:], 0x80000018)
	put(b[60:], 40)
	put(b[64:], 24)
	copy(b[80:], "private.dylib\x00")
	info, err := inspectNative(b)
	require.NoError(t, err)
	assert.Equal(t, []string{"private.dylib"}, info.imports)
	put(b[40:], 2)
	_, err = inspectNative(b)
	require.Error(t, err)
	put(b[40:], 1)
	put(b[64:], 1000)
	_, err = inspectNative(b)
	require.Error(t, err)
}

func syntheticMachO(fileType uint32, imports ...string) []byte {
	commandSizes := []int{24}
	for _, name := range imports {
		size := 24 + len(name) + 1
		commandSizes = append(commandSizes, (size+7)&^7)
	}
	sizeOfCommands := 0
	for _, size := range commandSizes {
		sizeOfCommands += size
	}
	data := make([]byte, 32+sizeOfCommands)
	put := binary.LittleEndian.PutUint32
	put(data, 0xfeedfacf)
	put(data[4:], uint32(macho.CpuAmd64))
	put(data[12:], fileType)
	put(data[16:], uint32(len(commandSizes)))
	put(data[20:], uint32(sizeOfCommands))
	offset := 32
	put(data[offset:], 0x32)
	put(data[offset+4:], 24)
	put(data[offset+8:], 1)
	offset += 24
	for i, name := range imports {
		put(data[offset:], 0x80000018)
		put(data[offset+4:], uint32(commandSizes[i+1]))
		put(data[offset+8:], 24)
		copy(data[offset+24:], name+"\x00")
		offset += commandSizes[i+1]
	}
	return data
}

func TestNestedBundleImportResolution(t *testing.T) {
	a := testArtifact("darwin")
	a.Kind = "bundle"
	a.Executable = "bin/app"
	a.SystemLibraries = nil
	a.RuntimeFiles = []string{"bin/private.dylib", "Frameworks/libfoo.dylib", "Frameworks/nested/libbar.dylib"}
	a.RequiredFiles = nil
	members := []testMember{
		{name: a.Executable, data: syntheticMachO(uint32(macho.TypeExec), "@loader_path/private.dylib", "@executable_path/../Frameworks/libfoo.dylib"), mode: 0o755},
		{name: "bin/private.dylib", data: syntheticMachO(uint32(macho.TypeDylib)), mode: 0o644},
		{name: "Frameworks/libfoo.dylib", data: syntheticMachO(uint32(macho.TypeDylib), "@loader_path/nested/libbar.dylib"), mode: 0o644},
		{name: "Frameworks/nested/libbar.dylib", data: syntheticMachO(uint32(macho.TypeDylib)), mode: 0o644},
	}
	dir, _ := archiveFixture(t, a, members)
	require.NoError(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
}

func TestDarwinArchiveRejectsCaseEquivalentPaths(t *testing.T) {
	a := testArtifact("darwin")
	executable := syntheticMachO(uint32(macho.TypeExec))
	valid := []testMember{
		{name: a.Executable, data: executable, mode: 0o755},
		{name: "NOTICE", data: []byte("notice"), mode: 0o644},
	}
	check := func(t *testing.T, artifact Artifact, members []testMember) error {
		t.Helper()
		dir, _ := archiveFixture(t, artifact, members)
		return Check(t.Context(), Contract{[]Artifact{artifact}}, dir, "1.2.3", "")
	}

	t.Run("exact contract spelling passes", func(t *testing.T) {
		require.NoError(t, check(t, a, valid))
	})
	t.Run("case alias cannot overwrite executable", func(t *testing.T) {
		members := append(append([]testMember{}, valid...), testMember{name: "APP", data: []byte("not native"), mode: 0o644})
		require.ErrorContains(t, check(t, a, members), "duplicate archive path")
	})
	t.Run("case alias cannot satisfy contract path", func(t *testing.T) {
		artifact := a
		artifact.Executable = "App"
		require.ErrorContains(t, check(t, artifact, valid), "required regular nonempty member missing: App")
	})
	t.Run("case alias cannot satisfy import path", func(t *testing.T) {
		artifact := a
		artifact.Kind = "bundle"
		artifact.Executable = "bin/app"
		artifact.RuntimeFiles = []string{"bin/Private.dylib"}
		artifact.RequiredFiles = nil
		artifact.SystemLibraries = nil
		members := []testMember{
			{name: artifact.Executable, data: syntheticMachO(uint32(macho.TypeExec), "@loader_path/private.dylib"), mode: 0o755},
			{name: "bin/Private.dylib", data: syntheticMachO(uint32(macho.TypeDylib)), mode: 0o644},
		}
		require.ErrorContains(t, check(t, artifact, members), "unresolved import")
	})
	for _, tc := range []struct {
		name      string
		conflicts []testMember
	}{
		{
			name: "file before case-alias child",
			conflicts: []testMember{
				{name: "Data", data: []byte("file"), mode: 0o644},
				{name: "data/item", data: []byte("child"), mode: 0o644},
			},
		},
		{
			name: "file after case-alias child",
			conflicts: []testMember{
				{name: "Data/item", data: []byte("child"), mode: 0o644},
				{name: "data", data: []byte("file"), mode: 0o644},
			},
		},
		{
			name: "directory aliases",
			conflicts: []testMember{
				{name: "Frameworks", mode: 0o755, typ: tar.TypeDir},
				{name: "frameworks", mode: 0o755, typ: tar.TypeDir},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			members := append(append([]testMember{}, valid...), tc.conflicts...)
			err := check(t, a, members)
			if tc.name == "directory aliases" {
				require.ErrorContains(t, err, "duplicate archive path")
				return
			}
			require.ErrorContains(t, err, "file is also directory prefix")
		})
	}
}

func TestBundleImportRequiresSharedLibraryImage(t *testing.T) {
	a := testArtifact("darwin")
	a.Kind = "bundle"
	a.SystemLibraries = nil
	a.RequiredFiles = nil
	a.RuntimeFiles = []string{"private.dylib"}
	app := syntheticMachO(uint32(macho.TypeExec), "@rpath/private.dylib")
	dylib := syntheticMachO(uint32(macho.TypeDylib))
	check := func(t *testing.T, artifact Artifact, members []testMember) error {
		t.Helper()
		dir, _ := archiveFixture(t, artifact, members)
		return Check(t.Context(), Contract{[]Artifact{artifact}}, dir, "1.2.3", "")
	}

	t.Run("shared library satisfies import", func(t *testing.T) {
		members := []testMember{
			{name: a.Executable, data: app, mode: 0o755},
			{name: "private.dylib", data: dylib, mode: 0o644},
		}
		require.NoError(t, check(t, a, members))
	})
	t.Run("executable cannot satisfy library import", func(t *testing.T) {
		members := []testMember{
			{name: a.Executable, data: app, mode: 0o755},
			{name: "private.dylib", data: syntheticMachO(uint32(macho.TypeExec)), mode: 0o755},
		}
		require.ErrorContains(t, check(t, a, members), "not a shared-library image")
	})
	t.Run("Mach-O bundle cannot satisfy dylib import", func(t *testing.T) {
		members := []testMember{
			{name: a.Executable, data: app, mode: 0o755},
			{name: "private.dylib", data: syntheticMachO(uint32(macho.TypeBundle)), mode: 0o644},
		}
		require.ErrorContains(t, check(t, a, members), "not a shared-library image")
	})
	t.Run("ELF PIE cannot satisfy shared object import", func(t *testing.T) {
		artifact := testArtifact("linux")
		artifact.Kind = "bundle"
		artifact.RuntimeFiles = []string{"private.so"}
		artifact.RequiredFiles = nil
		artifact.SystemLibraries = []string{"libc.so.6"}
		pie := syntheticELF("libc.so.6")
		binary.LittleEndian.PutUint16(pie[16:], uint16(elf.ET_DYN))
		binary.LittleEndian.PutUint64(pie[256+48:], uint64(elf.DT_FLAGS_1))
		binary.LittleEndian.PutUint64(pie[256+56:], uint64(elf.DF_1_PIE))
		binary.LittleEndian.PutUint64(pie[256+64:], uint64(elf.DT_NULL))
		binary.LittleEndian.PutUint64(pie[256+72:], 0)
		binary.LittleEndian.PutUint64(pie[120+32:], 80)
		binary.LittleEndian.PutUint64(pie[120+40:], 80)
		members := []testMember{
			{name: artifact.Executable, data: syntheticELF("private.so"), mode: 0o755},
			{name: "private.so", data: pie, mode: 0o755},
		}
		require.ErrorContains(t, check(t, artifact, members), "not a shared-library image")
	})
	t.Run("PE executable cannot satisfy DLL import", func(t *testing.T) {
		artifact := testArtifact("windows")
		artifact.Kind = "bundle"
		artifact.RuntimeFiles = []string{"private.dll"}
		artifact.RequiredFiles = nil
		artifact.SystemLibraries = nil
		members := []testMember{
			{name: artifact.Executable, data: syntheticPE(true), mode: 0o755},
			{name: "private.dll", data: syntheticPE(true), mode: 0o755},
		}
		require.ErrorContains(t, check(t, artifact, members), "not a shared-library image")
	})
	t.Run("standalone executable runtime helper remains valid", func(t *testing.T) {
		artifact := a
		artifact.RuntimeFiles = []string{"helper"}
		members := []testMember{
			{name: artifact.Executable, data: syntheticMachO(uint32(macho.TypeExec)), mode: 0o755},
			{name: "helper", data: syntheticMachO(uint32(macho.TypeExec)), mode: 0o755},
		}
		require.NoError(t, check(t, artifact, members))
	})
}

func TestBundledImportPrefixesAreTargetSpecific(t *testing.T) {
	t.Run("Linux does not expand Darwin loader prefixes", func(t *testing.T) {
		for _, prefix := range []string{"@rpath/", "@loader_path/", "@executable_path/"} {
			t.Run(strings.TrimSuffix(prefix, "/"), func(t *testing.T) {
				a := testArtifact("linux")
				a.Kind = "bundle"
				a.Executable = "app"
				a.RuntimeFiles = []string{"private.so"}
				a.RequiredFiles = nil
				a.SystemLibraries = []string{"libc.so.6"}
				library := syntheticELF("libc.so.6")
				binary.LittleEndian.PutUint16(library[16:], uint16(elf.ET_DYN))
				members := []testMember{
					{name: a.Executable, data: syntheticELF(prefix + "private.so"), mode: 0o755},
					{name: "private.so", data: library, mode: 0o644},
				}
				dir, _ := archiveFixture(t, a, members)
				require.ErrorContains(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""), "unresolved import")
			})
		}
	})
	t.Run("Linux expands both ORIGIN forms from the importing member", func(t *testing.T) {
		for _, prefix := range []string{"$ORIGIN/", "${ORIGIN}/"} {
			t.Run(strings.TrimSuffix(prefix, "/"), func(t *testing.T) {
				a := testArtifact("linux")
				a.Kind = "bundle"
				a.Executable = "bin/app"
				a.RuntimeFiles = []string{"bin/private.so"}
				a.RequiredFiles = nil
				a.SystemLibraries = []string{"libc.so.6"}
				library := syntheticELF("libc.so.6")
				binary.LittleEndian.PutUint16(library[16:], uint16(elf.ET_DYN))
				members := []testMember{
					{name: a.Executable, data: syntheticELF(prefix + "private.so"), mode: 0o755},
					{name: "bin/private.so", data: library, mode: 0o644},
				}
				dir, _ := archiveFixture(t, a, members)
				require.NoError(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
			})
		}
	})
	t.Run("Darwin does not expand Linux ORIGIN forms", func(t *testing.T) {
		for _, prefix := range []string{"$ORIGIN/", "${ORIGIN}/"} {
			t.Run(strings.TrimSuffix(prefix, "/"), func(t *testing.T) {
				a := testArtifact("darwin")
				a.Kind = "bundle"
				a.RuntimeFiles = []string{"private.dylib"}
				a.RequiredFiles = nil
				a.SystemLibraries = nil
				members := []testMember{
					{name: a.Executable, data: syntheticMachO(uint32(macho.TypeExec), prefix+"private.dylib"), mode: 0o755},
					{name: "private.dylib", data: syntheticMachO(uint32(macho.TypeDylib)), mode: 0o644},
				}
				dir, _ := archiveFixture(t, a, members)
				require.ErrorContains(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""), "unresolved import")
			})
		}
	})
}

func TestUnixExecutableAndHelperRequireOwnerExecute(t *testing.T) {
	for _, targetOS := range []string{"linux", "darwin"} {
		t.Run(targetOS, func(t *testing.T) {
			data := syntheticMachO(uint32(macho.TypeExec))
			systemLibraries := []string(nil)
			if targetOS == "linux" {
				data = syntheticELF("libc.so.6")
				systemLibraries = []string{"libc.so.6"}
			}
			for _, role := range []string{"executable", "helper"} {
				t.Run(role, func(t *testing.T) {
					for _, tc := range []struct {
						name string
						mode int64
					}{
						{name: "group execute only", mode: 0o654},
						{name: "other execute only", mode: 0o645},
					} {
						t.Run(tc.name, func(t *testing.T) {
							a := testArtifact(targetOS)
							a.SystemLibraries = systemLibraries
							members := []testMember{
								{name: a.Executable, data: data, mode: 0o755},
								{name: "NOTICE", data: []byte("notice"), mode: 0o644},
							}
							if role == "helper" {
								a.Kind = "bundle"
								a.RuntimeFiles = []string{"helper"}
								members = append(members, testMember{name: "helper", data: data, mode: tc.mode})
							} else {
								members[0].mode = tc.mode
							}
							dir, _ := archiveFixture(t, a, members)
							require.ErrorContains(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""), "lacks Unix owner execute permission")
						})
					}
					t.Run("owner execute passes", func(t *testing.T) {
						a := testArtifact(targetOS)
						a.SystemLibraries = systemLibraries
						members := []testMember{
							{name: a.Executable, data: data, mode: 0o755},
							{name: "NOTICE", data: []byte("notice"), mode: 0o644},
						}
						if role == "helper" {
							a.Kind = "bundle"
							a.RuntimeFiles = []string{"helper"}
							members = append(members, testMember{name: "helper", data: data, mode: 0o755})
						}
						dir, _ := archiveFixture(t, a, members)
						require.NoError(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
					})
				})
			}
		})
	}
}

func TestMalformedDuplicateChecksum(t *testing.T) {
	a := testArtifact("linux")
	dir, name := archiveFixture(t, a, nil)
	p := filepath.Join(dir, "SHA256SUMS")
	original, err := os.ReadFile(p)
	require.NoError(t, err)
	malformed := fmt.Sprintf("%s  %s unexpected\n", strings.Repeat("0", 64), name)
	require.NoError(t, os.WriteFile(p, append(original, []byte(malformed)...), 0o600))
	a.Name = name
	_, err = readChecksums(t.Context(), dir, []Artifact{a})
	require.Error(t, err)
}

func TestPEMixedOrdinalAndNamedDescriptors(t *testing.T) {
	data := syntheticPE(true)
	// Keep the first DLL ordinal-only and add a second descriptor importing a
	// named symbol. A symbol-based dependency scan would omit the first DLL.
	binary.LittleEndian.PutUint32(data[0x84+20+124:], 60)
	desc := data[0x214:]
	binary.LittleEndian.PutUint32(desc, 0x10b0)
	binary.LittleEndian.PutUint32(desc[12:], 0x1070)
	binary.LittleEndian.PutUint32(desc[16:], 0x10b0)
	copy(data[0x270:], "named.dll\x00")
	binary.LittleEndian.PutUint64(data[0x2b0:], 0x10a0)
	copy(data[0x2a2:], "named_function\x00")
	info, err := inspectNative(data)
	require.NoError(t, err)
	assert.Equal(t, []string{"private.dll", "named.dll"}, info.imports)
	binary.LittleEndian.PutUint32(data[0x84+20+124:], 20)
	_, err = inspectNative(data)
	require.ErrorContains(t, err, "unterminated")
}

func TestPythonTarRootDirectory(t *testing.T) {
	// shutil.make_archive(..., "gztar", bundle) emits a literal "." directory,
	// followed by "./name" members. It is a harmless archive root, not traversal.
	a := testArtifact("linux")
	a.RequiredFiles = nil
	a.SystemLibraries = []string{"private.so"}
	members := []testMember{{".", nil, 0o755, tar.TypeDir}, {"./" + a.Executable, syntheticELF("private.so"), 0o755, 0}}
	dir, _ := archiveFixture(t, a, members)
	require.NoError(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
}

func elfWithInterpreter(loader string) []byte {
	b := syntheticELF("system.so")
	// Leave PT_LOAD intact, and replace PT_DYNAMIC with PT_INTERP. No DT_NEEDED
	// entries remain, so this isolates the interpreter as a runtime dependency.
	p := b[120:]
	binary.LittleEndian.PutUint32(p, 3)
	binary.LittleEndian.PutUint64(p[8:], 256)
	binary.LittleEndian.PutUint64(p[32:], uint64(len(loader)+1))
	binary.LittleEndian.PutUint64(p[40:], uint64(len(loader)+1))
	clear(b[256:])
	copy(b[256:], loader)
	return b
}

func TestELFInterpreterPolicy(t *testing.T) {
	const loader = "/lib64/ld-linux-x86-64.so.2"
	for _, kind := range []string{"single", "bundle"} {
		t.Run(kind, func(t *testing.T) {
			a := testArtifact("linux")
			a.Kind = kind
			a.RequiredFiles = nil
			members := []testMember{{a.Executable, elfWithInterpreter(loader), 0o755, 0}}
			if kind == "bundle" {
				a.RuntimeFiles = []string{"helper"}
				members = append(members, testMember{"helper", syntheticELF("system.so"), 0o755, 0})
			}
			a.SystemLibraries = []string{"ld-linux-x86-64.so.2", "system.so"}
			dir, _ := archiveFixture(t, a, members)
			require.ErrorContains(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""), loader)
			a.SystemLibraries = append(a.SystemLibraries, loader)
			require.NoError(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
			members[0].data = elfWithInterpreter("/opt/custom/ld-linux-x86-64.so.2")
			dir, _ = archiveFixture(t, a, members)
			require.Error(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
		})
	}
	for _, loader := range []string{"ld-linux.so", "/lib/../custom/ld.so", "/lib//ld.so", "/lib/ld.so\x00hidden", "/lib/ld\\bad.so"} {
		t.Run(loader, func(t *testing.T) { _, err := inspectNative(elfWithInterpreter(loader)); require.Error(t, err) })
	}
}

func TestPEWindowsApplicationSubsystem(t *testing.T) {
	for _, machine := range []uint16{0x8664, 0xaa64} {
		for _, dll := range []bool{false, true} {
			data := syntheticPE(true)
			binary.LittleEndian.PutUint16(data[0x84:], machine)
			if dll {
				binary.LittleEndian.PutUint16(data[0x84+18:], 0x2022)
			}
			for _, subsystem := range []uint16{2, 3} {
				binary.LittleEndian.PutUint16(data[0x84+20+68:], subsystem)
				_, err := inspectNative(data)
				require.NoError(t, err)
			}
			// EFI application, native/driver, unknown, POSIX, and Windows boot images
			// are PE files but do not run as ordinary Windows release applications.
			for _, subsystem := range []uint16{10, 1, 0, 7, 16} {
				binary.LittleEndian.PutUint16(data[0x84+20+68:], subsystem)
				_, err := inspectNative(data)
				require.Error(t, err, "machine=%x dll=%v subsystem=%d", machine, dll, subsystem)
			}
		}
	}
}

func TestWindowsMemberSafety(t *testing.T) {
	a := testArtifact("windows")
	a.SystemLibraries = []string{"private.dll"}
	a.RequiredFiles = nil
	names := []string{"LICENSE.", "LICENSE ", "nested./file", "nested /file", "NUL.txt", "sub/CON", "AUX.log", "PRN", "COM1.txt", "com9", "LPT1.log", "lpt9", "COM¹.log", "LPT³", "dir/a<b", "dir/a>b", "dir/a\"b", "dir/a|b", "dir/a?b", "dir/a*b"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			members := []testMember{{a.Executable, syntheticPE(true), 0o755, 0}, {name, []byte("payload"), 0o644, 0}}
			dir, _ := archiveFixture(t, a, members)
			require.Error(t, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
			invalid := a
			invalid.RequiredFiles = []string{name}
			raw, err := json.Marshal(Contract{[]Artifact{invalid}})
			require.NoError(t, err)
			_, err = ReadContract(bytes.NewReader(raw))
			require.Error(t, err)
		})
	}
	for _, name := range []string{"normal.txt", "nested/path.txt", "COM10.txt", "LPT10", "NUL-safe.txt", "my file.txt"} {
		t.Run(name, func(t *testing.T) {
			members := []testMember{{a.Executable, syntheticPE(true), 0o755, 0}, {name, []byte("payload"), 0o644, 0}}
			valid := a
			valid.RequiredFiles = []string{name}
			dir, _ := archiveFixture(t, valid, members)
			require.NoError(t, Check(t.Context(), Contract{[]Artifact{valid}}, dir, "1.2.3", ""))
		})
	}
}

func TestWindowsTrailingAliasProperty(t *testing.T) {
	a := testArtifact("windows")
	a.SystemLibraries = []string{"private.dll"}
	a.RequiredFiles = nil
	rapid.Check(t, func(rt *rapid.T) {
		stem := rapid.StringMatching(`[a-zA-Z0-9][a-zA-Z0-9_-]*`).Draw(rt, "stem")
		if len(stem) > 128 {
			stem = stem[:128]
		} // Bound fixture materialization, not the input domain.
		stem = "payload-" + stem // Prefix keeps every generated stem non-reserved.
		suffix := rapid.SampledFrom([]string{".", " "}).Draw(rt, "alias suffix")
		members := []testMember{{a.Executable, syntheticPE(true), 0o755, 0}, {stem, []byte("original"), 0o644, 0}}
		dir, _ := archiveFixture(t, a, members)
		require.NoError(rt, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
		members = append(members, testMember{stem + suffix, []byte("alias"), 0o644, 0})
		dir, _ = archiveFixture(t, a, members)
		require.Error(rt, Check(t.Context(), Contract{[]Artifact{a}}, dir, "1.2.3", ""))
	})
}
