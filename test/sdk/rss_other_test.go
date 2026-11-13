//go:build !windows && !linux && !darwin

package sdk_test

import (
	"runtime"
	"time"
)

type preciseClock struct{}

func (preciseClock) now() int64 {
	return time.Now().UnixNano()
}

func nowPrecise() int64 {
	return time.Now().UnixNano()
}

// sampleProcessRSS falls back to runtime.MemStats.Sys on platforms without OS RSS APIs.
func sampleProcessRSS() (currentRSS uint64, peakRSS uint64, ok bool) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.Sys, ms.Sys, true
}
