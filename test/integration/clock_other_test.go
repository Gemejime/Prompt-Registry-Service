//go:build !windows

package integration_test

import "time"

type preciseClock struct{}

func (preciseClock) now() int64 {
	return time.Now().UnixNano()
}
