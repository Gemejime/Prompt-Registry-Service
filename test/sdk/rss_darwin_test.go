//go:build darwin

package sdk_test

import (
	"syscall"
	"time"
)

type preciseClock struct{}

func (preciseClock) now() int64 {
	return time.Now().UnixNano()
}

func nowPrecise() int64 {
	return time.Now().UnixNano()
}

// sampleProcessRSS returns process peak RSS bytes using getrusage on macOS.
func sampleProcessRSS() (currentRSS uint64, peakRSS uint64, ok bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, 0, false
	}
	rss := uint64(ru.Maxrss)
	return rss, rss, true
}
