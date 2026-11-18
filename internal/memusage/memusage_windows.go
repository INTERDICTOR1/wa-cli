//go:build windows

package memusage

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// processMemoryCountersEx2 is PROCESS_MEMORY_COUNTERS_EX2 (Windows 10
// 1809 and later), whose PrivateWorkingSetSize is Task Manager's figure.
type processMemoryCountersEx2 struct {
	cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
	PrivateWorkingSetSize      uintptr
	SharedCommitUsage          uintptr
}

// processMemoryCounters is the older PROCESS_MEMORY_COUNTERS, for
// Windows versions that don't know the EX2 form.
type processMemoryCounters struct {
	cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var getProcessMemoryInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

func current() (uint64, bool) {
	if getProcessMemoryInfo.Find() != nil {
		return 0, false
	}
	self := windows.CurrentProcess()

	var ex2 processMemoryCountersEx2
	ex2.cb = uint32(unsafe.Sizeof(ex2))
	if ok, _, _ := getProcessMemoryInfo.Call(uintptr(self), uintptr(unsafe.Pointer(&ex2)), uintptr(ex2.cb)); ok != 0 && ex2.PrivateWorkingSetSize > 0 {
		return uint64(ex2.PrivateWorkingSetSize), true
	}

	// Older Windows: the whole working set, shared pages included.
	var pmc processMemoryCounters
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	if ok, _, _ := getProcessMemoryInfo.Call(uintptr(self), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb)); ok != 0 {
		return uint64(pmc.WorkingSetSize), true
	}
	return 0, false
}
