//go:build linux

package sdk_test

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type preciseClock struct{}

func (preciseClock) now() int64 {
	return time.Now().UnixNano()
}

func nowPrecise() int64 {
	return time.Now().UnixNano()
}

// sampleProcessRSS reads VmRSS and VmHWM from /proc/self/status on Linux.
func sampleProcessRSS() (currentRSS uint64, peakRSS uint64, ok bool) {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, 0, false
	}
	var gotCur, gotPeak bool
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			if kb, err := parseProcKB(line); err == nil {
				currentRSS = kb * 1024
				gotCur = true
			}
		} else if strings.HasPrefix(line, "VmHWM:") {
			if kb, err := parseProcKB(line); err == nil {
				peakRSS = kb * 1024
				gotPeak = true
			}
		}
	}
	if !gotCur && !gotPeak {
		return 0, 0, false
	}
	if peakRSS < currentRSS {
		peakRSS = currentRSS
	}
	return currentRSS, peakRSS, true
}

func parseProcKB(line string) (uint64, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, strconv.ErrSyntax
	}
	return strconv.ParseUint(fields[1], 10, 64)
}
