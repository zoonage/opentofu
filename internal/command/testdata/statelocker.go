// statelocker use used for testing command with a locked state.
// This will lock the state file at a given path, then wait for a signal. On
// SIGINT and SIGTERM the state will be Unlocked before exit.
package main

import (
	"context"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/opentofu/opentofu/internal/command/clistate"
	"github.com/opentofu/opentofu/internal/states/statemgr"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal(os.Args[0], "statefile")
	}

	s := &clistate.LocalState{
		Path: os.Args[1],
	}

	info := statemgr.NewLockInfo()
	info.Operation = "test"
	info.Info = "state locker"

	lockID, err := s.Lock(context.Background(), info)
	if err != nil {
		io.WriteString(os.Stderr, err.Error())
		return
	}

	// signal to the caller that we're locked
	io.WriteString(os.Stdout, "LOCKID "+lockID)

	defer func() {
		if err := s.Unlock(context.Background(), lockID); err != nil {
			io.WriteString(os.Stderr, err.Error())
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	// timeout after 10 second in case we don't get cleaned up by the test
	select {
	case <-time.After(10 * time.Second):
	case <-c:
	}
}
