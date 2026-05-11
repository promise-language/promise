//go:build !windows

package common

import (
	"os"
	"os/signal"
	"sync/atomic"
)

var interrupted atomic.Int32

func init() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	go func() {
		<-ch
		interrupted.Store(1)
	}()
}

// Interrupted returns true if the user has pressed Ctrl+C.
func Interrupted() bool {
	return interrupted.Load() != 0
}
