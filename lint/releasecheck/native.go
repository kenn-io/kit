package releasecheck

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"errors"
	"fmt"
)

type nativeInfo struct {
	os, arch                           string
	executable, library, sharedLibrary bool
	interp                             string
	imports                            []string
}

func nativeMagic(data []byte) bool {
	if bytes.HasPrefix(data, []byte("\x7fELF")) || bytes.HasPrefix(data, []byte("MZ")) {
		return true
	}
	if len(data) < 4 {
		return false
	}
	switch binary.BigEndian.Uint32(data) {
	case 0xfeedface, 0xfeedfacf, 0xcefaedfe, 0xcffaedfe, 0xcafebabe, 0xbebafeca, 0xcafebabf, 0xbfbafeca:
		return true
	}
	return false
}

func inspectNative(data []byte) (nativeInfo, error) {
	if bytes.HasPrefix(data, []byte("\x7fELF")) {
		return inspectELF(data)
	}
	if bytes.HasPrefix(data, []byte("MZ")) {
		return inspectPE(data)
	}
	if nativeMagic(data) {
		return inspectMachO(data)
	}
	return nativeInfo{}, errors.New("not a recognized native image")
}

func inspectELF(data []byte) (nativeInfo, error) {
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return nativeInfo{}, err
	}
	defer f.Close()
	info := nativeInfo{os: "linux"}
	if f.ByteOrder != binary.LittleEndian || f.Class != elf.ELFCLASS64 {
		return info, errors.New("unsupported ELF class/byte order")
	}
	switch f.Machine {
	case elf.EM_X86_64:
		info.arch = "amd64"
	case elf.EM_AARCH64:
		info.arch = "arm64"
	default:
		return info, fmt.Errorf("unsupported ELF machine %s", f.Machine)
	}
	// System V and Linux identify supported Linux releases; other ELF OS ABIs
	// (e.g. FreeBSD) must not be mislabeled as Linux merely because they are ELF.
	if f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX {
		return info, fmt.Errorf("unsupported ELF OS ABI %s", f.OSABI)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			if info.interp != "" || p.Filesz < 2 || p.Filesz > 4096 || p.Off > uint64(len(data)) || p.Filesz > uint64(len(data))-p.Off {
				return info, errors.New("invalid ELF interpreter bounds")
			}
			raw := data[p.Off : p.Off+p.Filesz]
			end := bytes.IndexByte(raw, 0)
			if end != len(raw)-1 || raw[0] != '/' || !memberPath(string(raw[1:end])) {
				return info, errors.New("ELF interpreter must be one canonical absolute path")
			}
			info.interp = string(raw[:end])
		}
	}
	imports, soname, pie, err := elfDynamic(f, data)
	if err != nil {
		return info, err
	}
	info.imports = imports
	if info.interp != "" {
		info.imports = append(info.imports, info.interp)
	}
	info.library = f.Type == elf.ET_DYN
	info.sharedLibrary = f.Type == elf.ET_DYN && !pie && info.interp == ""
	info.executable = (f.Type == elf.ET_EXEC || f.Type == elf.ET_DYN && (info.interp != "" || pie)) && f.Entry != 0 && !soname
	if !info.library && !info.executable {
		return info, errors.New("ELF is not an executable or shared library")
	}
	return info, nil
}

func inspectMachO(data []byte) (nativeInfo, error) {
	f, err := macho.NewFile(bytes.NewReader(data))
	if err != nil {
		return nativeInfo{}, err
	}
	defer f.Close()
	info := nativeInfo{
		os:            "darwin",
		executable:    f.Type == macho.TypeExec,
		library:       f.Type == macho.TypeDylib || f.Type == macho.TypeBundle,
		sharedLibrary: f.Type == macho.TypeDylib,
	}
	switch f.Cpu {
	case macho.CpuAmd64:
		info.arch = "amd64"
	case macho.CpuArm64:
		info.arch = "arm64"
	default:
		return info, fmt.Errorf("unsupported Mach-O CPU %s", f.Cpu)
	}
	if !info.executable && !info.library {
		return info, errors.New("Mach-O is not an executable or shared library")
	}
	// debug/macho decodes LC_LOAD_DYLIB only. Inspect raw commands as well for
	// weak/reexport/upward/lazy imports, which also name runtime dependencies.
	for _, load := range f.Loads {
		raw := load.Raw()
		if len(raw) < 8 {
			return info, errors.New("truncated Mach-O load command")
		}
		cmd := f.ByteOrder.Uint32(raw) & 0x7fffffff
		switch cmd {
		case 0x32: // LC_BUILD_VERSION
			if len(raw) < 24 || f.ByteOrder.Uint32(raw[8:]) != 1 {
				return info, errors.New("Mach-O platform is not macOS")
			}
		case 0x25, 0x2f, 0x30: // Legacy iOS, tvOS, watchOS deployment commands.
			return info, errors.New("Mach-O platform is not macOS")
		case 0xc, 0x18, 0x1f, 0x20, 0x23:
			if len(raw) < 24 {
				return info, errors.New("truncated Mach-O dylib command")
			}
			off := f.ByteOrder.Uint32(raw[8:])
			if off < 24 || uint64(off) >= uint64(len(raw)) {
				return info, errors.New("invalid Mach-O dylib name offset")
			}
			name, err := terminatedName(raw[off:])
			if err != nil {
				return info, err
			}
			info.imports = append(info.imports, name)
		}
	}
	return info, nil
}

func inspectPE(data []byte) (nativeInfo, error) {
	f, err := pe.NewFile(bytes.NewReader(data))
	if err != nil {
		return nativeInfo{}, err
	}
	defer f.Close()
	info := nativeInfo{os: "windows"}
	switch f.Machine {
	case pe.IMAGE_FILE_MACHINE_AMD64:
		info.arch = "amd64"
	case pe.IMAGE_FILE_MACHINE_ARM64:
		info.arch = "arm64"
	default:
		return info, fmt.Errorf("unsupported PE machine %#x", f.Machine)
	}
	optional, ok := f.OptionalHeader.(*pe.OptionalHeader64)
	if !ok || optional.Magic != 0x20b {
		return info, errors.New("PE requires a PE32+ optional header")
	}
	if optional.Subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_GUI && optional.Subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_CUI {
		return info, errors.New("PE subsystem is not a Windows GUI or console application")
	}
	if f.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE == 0 {
		return info, errors.New("PE is not an executable image")
	}
	info.library = f.Characteristics&pe.IMAGE_FILE_DLL != 0
	info.sharedLibrary = info.library
	info.executable = !info.library && optional.AddressOfEntryPoint != 0
	if !info.library && !info.executable {
		return info, errors.New("PE executable has no entry point")
	}
	if optional.NumberOfRvaAndSizes <= 1 {
		return info, nil
	}
	directory := optional.DataDirectory[1]
	if directory.VirtualAddress == 0 && directory.Size == 0 {
		return info, nil
	}
	if directory.VirtualAddress == 0 || directory.Size < 20 {
		return info, errors.New("invalid PE import directory")
	}
	// RVA mapping is restricted to file-backed section bytes. Never turn a bad
	// RVA into an unchecked slice or silently omit a DLL because it has no named
	// symbols (ordinal-only descriptors are ordinary dependencies too).
	rva := func(address uint32) ([]byte, error) {
		for _, s := range f.Sections {
			start := uint64(s.VirtualAddress)
			addr := uint64(address)
			if addr < start {
				continue
			}
			off := addr - start
			if off >= uint64(s.Size) {
				continue
			}
			pos := uint64(s.Offset) + off
			remaining := uint64(s.Size) - off
			if pos > uint64(len(data)) || remaining > uint64(len(data))-pos {
				return nil, errors.New("PE import RVA outside file")
			}
			return data[pos : pos+remaining], nil
		}
		return nil, fmt.Errorf("unmapped PE import RVA %#x", address)
	}
	table, err := rva(directory.VirtualAddress)
	if err != nil {
		return info, err
	}
	if uint64(directory.Size) > uint64(len(table)) {
		return info, errors.New("PE import directory exceeds file-backed section")
	}
	table = table[:directory.Size]
	for off := 0; off+20 <= len(table); off += 20 {
		if off/20 > 65536 {
			return info, errors.New("too many PE import descriptors")
		}
		desc := table[off : off+20]
		if bytes.Equal(desc, make([]byte, 20)) {
			return info, nil
		}
		nameRVA := binary.LittleEndian.Uint32(desc[12:])
		if nameRVA == 0 {
			return info, errors.New("PE import descriptor has no DLL name")
		}
		raw, err := rva(nameRVA)
		if err != nil {
			return info, err
		}
		name, err := terminatedName(raw)
		if err != nil {
			return info, err
		}
		info.imports = append(info.imports, name)
	}
	return info, errors.New("unterminated PE import descriptor table")
}

func terminatedName(raw []byte) (string, error) {
	if len(raw) > 65536 {
		raw = raw[:65536]
	}
	end := bytes.IndexByte(raw, 0)
	if end <= 0 {
		return "", errors.New("empty or unterminated native import name")
	}
	return string(raw[:end]), nil
}

// Program headers remain available when ELF section headers have been stripped.
// debug/elf.ImportedLibraries reads only sections and silently skips invalid
// string offsets, so use the actual runtime dynamic table instead.
func elfDynamic(f *elf.File, data []byte) (imports []string, soname, pie bool, err error) {
	var dynamic *elf.Prog
	for _, p := range f.Progs {
		if p.Type == elf.PT_DYNAMIC {
			if dynamic != nil {
				return nil, false, false, errors.New("multiple ELF dynamic tables")
			}
			dynamic = p
		}
	}
	if dynamic == nil {
		if f.SectionByType(elf.SHT_DYNAMIC) != nil {
			return nil, false, false, errors.New("ELF dynamic section has no runtime program header")
		}
		return nil, false, false, nil
	}
	if dynamic.Filesz > uint64(len(data)) || dynamic.Off > uint64(len(data))-dynamic.Filesz || dynamic.Filesz%16 != 0 {
		return nil, false, false, errors.New("invalid ELF dynamic table bounds")
	}
	raw := data[dynamic.Off : dynamic.Off+dynamic.Filesz]
	var needed []uint64
	var strtab, strsize uint64
	terminated := false
	for len(raw) >= 16 {
		tag, value := f.ByteOrder.Uint64(raw), f.ByteOrder.Uint64(raw[8:])
		raw = raw[16:]
		if tag == uint64(elf.DT_NULL) {
			terminated = true
			break
		}
		switch tag {
		case uint64(elf.DT_NEEDED):
			needed = append(needed, value)
		case uint64(elf.DT_STRTAB):
			strtab = value
		case uint64(elf.DT_STRSZ):
			strsize = value
		case uint64(elf.DT_SONAME):
			soname = true
		case uint64(elf.DT_FLAGS_1):
			pie = value&uint64(elf.DF_1_PIE) != 0
		}
	}
	if !terminated {
		return nil, false, false, errors.New("unterminated ELF dynamic table")
	}
	if len(needed) == 0 {
		return nil, soname, pie, nil
	}
	if strsize == 0 {
		return nil, false, false, errors.New("ELF imports have no string table")
	}
	var stringsData []byte
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || strtab < p.Vaddr {
			continue
		}
		offset := strtab - p.Vaddr
		if offset > p.Filesz || strsize > p.Filesz-offset {
			continue
		}
		if p.Off > uint64(len(data)) || offset > uint64(len(data))-p.Off {
			continue
		}
		start := p.Off + offset
		if strsize > uint64(len(data))-start {
			continue
		}
		stringsData = data[start : start+strsize]
		break
	}
	if stringsData == nil {
		return nil, false, false, errors.New("ELF import string table is not file-backed")
	}
	for _, offset := range needed {
		if offset >= uint64(len(stringsData)) {
			return nil, false, false, errors.New("ELF import string offset out of bounds")
		}
		name, e := terminatedName(stringsData[offset:])
		if e != nil {
			return nil, false, false, e
		}
		imports = append(imports, name)
	}
	return imports, soname, pie, nil
}
