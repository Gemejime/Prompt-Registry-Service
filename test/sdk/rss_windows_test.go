//go:build windows

package sdk_test

import (
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32DLL             = syscall.NewLazyDLL("kernel32.dll")
	procGetCurrentProcess   = kernel32DLL.NewProc("GetCurrentProcess")
	procK32GetProcessMemInf = kernel32DLL.NewProc("K32GetProcessMemoryInfo")
	procQueryPerfCounter    = kernel32DLL.NewProc("QueryPerformanceCounter")
	procQueryPerfFreq       = kernel32DLL.NewProc("QueryPerformanceFrequency")
	qpcAddr                 uintptr
	qpcFreq                 int64
)

func init() {
	qpcAddr = procQueryPerfCounter.Addr()
	_, _, _ = procQueryPerfFreq.Call(uintptr(unsafe.Pointer(&qpcFreq)))
}

// preciseClock provides zero-heap-allocation monotonic timestamps per worker via QueryPerformanceCounter.
type preciseClock struct {
	cnt int64
}

func (c *preciseClock) now() int64 {
	if qpcAddr == 0 || qpcFreq <= 0 {
		return time.Now().UnixNano()
	}
	syscall.SyscallN(qpcAddr, uintptr(unsafe.Pointer(&c.cnt)))
	// Avoid overflow: (cnt / qpcFreq)*1e9 + (cnt % qpcFreq)*1e9/qpcFreq
	secs := c.cnt / qpcFreq
	rem := c.cnt % qpcFreq
	return secs*1_000_000_000 + (rem*1_000_000_000)/qpcFreq
}

// nowPrecise returns a high-resolution monotonic timestamp in nanoseconds via QueryPerformanceCounter.
func nowPrecise() int64 {
	var clk preciseClock
	return clk.now()
}

type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

// sampleProcessRSS returns the current and kernel-tracked peak working set (RSS) bytes on Windows.
func sampleProcessRSS() (currentRSS uint64, peakRSS uint64, ok bool) {
	handle, _, _ := procGetCurrentProcess.Call()
	var pmc processMemoryCounters
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	ret, _, _ := procK32GetProcessMemInf.Call(handle, uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb))
	if ret == 0 {
		return 0, 0, false
	}
	return uint64(pmc.workingSetSize), uint64(pmc.peakWorkingSetSize), true
}
