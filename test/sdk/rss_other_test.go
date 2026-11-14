//go:build !windows && !linux && !darwin

package sdk_test

import (
	"time"
)

type preciseClock struct{}

func (preciseClock) now() int64 {
	return time.Now().UnixNano()
}

func nowPrecise() int64 {
	return time.Now().UnixNano()
}

// sampleProcessRSS returns unavailable on platforms without OS RSS APIs.
func sampleProcessRSS() (currentRSS uint64, peakRSS uint64, ok bool) {
	return 0, 0, false
}
