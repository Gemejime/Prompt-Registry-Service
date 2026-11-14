//go:build windows

package integration_test

import (
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32DLL          = syscall.NewLazyDLL("kernel32.dll")
	procQueryPerfCounter = kernel32DLL.NewProc("QueryPerformanceCounter")
	procQueryPerfFreq    = kernel32DLL.NewProc("QueryPerformanceFrequency")
	qpcAddr              uintptr
	qpcFreq              int64
)

func init() {
	qpcAddr = procQueryPerfCounter.Addr()
	_, _, _ = procQueryPerfFreq.Call(uintptr(unsafe.Pointer(&qpcFreq)))
}

// preciseClock provides sub-microsecond monotonic timestamps via QueryPerformanceCounter on Windows.
type preciseClock struct {
	cnt int64
}

func (c *preciseClock) now() int64 {
	if qpcAddr == 0 || qpcFreq <= 0 {
		return time.Now().UnixNano()
	}
	syscall.SyscallN(qpcAddr, uintptr(unsafe.Pointer(&c.cnt)))
	secs := c.cnt / qpcFreq
	rem := c.cnt % qpcFreq
	return secs*1_000_000_000 + (rem*1_000_000_000)/qpcFreq
}
