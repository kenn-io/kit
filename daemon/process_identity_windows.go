//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// listedProcessCreateTime reads pid's creation time, in Unix milliseconds,
// from the system process list. Unlike opening the process, the list needs no
// access to it, so it also covers processes of other accounts and services.
func listedProcessCreateTime(pid int) (int64, error) {
	size := uint32(512 << 10)
	for range 5 {
		// uint64 elements keep the entries 8-byte aligned.
		buf := make([]uint64, size/8+1)
		var needed uint32
		err := windows.NtQuerySystemInformation(
			windows.SystemProcessInformation,
			unsafe.Pointer(&buf[0]),
			uint32(len(buf)*8),
			&needed,
		)
		if errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) {
			// The list can grow between calls, so ask for headroom.
			size = max(needed, size) * 2
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("list processes: %w", err)
		}
		return findListedProcessCreateTime(unsafe.Pointer(&buf[0]), pid)
	}
	return 0, errors.New("list processes: process list kept growing")
}

func findListedProcessCreateTime(list unsafe.Pointer, pid int) (int64, error) {
	for {
		info := (*windows.SYSTEM_PROCESS_INFORMATION)(list)
		if info.UniqueProcessID == uintptr(pid) {
			created := windows.Filetime{
				LowDateTime:  uint32(info.CreateTime),
				HighDateTime: uint32(info.CreateTime >> 32),
			}
			// Same conversion as the opened-process path, so both read
			// the same identity.
			return created.Nanoseconds() / 1_000_000, nil
		}
		if info.NextEntryOffset == 0 {
			return 0, fmt.Errorf("process %d is not listed", pid)
		}
		list = unsafe.Add(list, info.NextEntryOffset)
	}
}
