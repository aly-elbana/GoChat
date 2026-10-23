//go:build unix

package tui

import (
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
)

// On Unix we switch the terminal into "cbreak" mode with stty: no line
// buffering and no echo, but signals stay on, so Ctrl+C still raises SIGINT
// and the program can shut down gracefully. Using the stty tool keeps this
// package free of platform-specific ioctl numbers (Linux, macOS, BSD alike).

func stty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func enableRaw(_ *os.File) (func() error, error) {
	saved, err := stty("-g")
	if err != nil {
		return nil, err
	}
	if _, err := stty("-icanon", "-echo", "min", "1", "time", "0"); err != nil {
		return nil, err
	}
	return func() error {
		_, err := stty(saved)
		return err
	}, nil
}

// EnableColor reports whether ANSI codes can be used on f.
func EnableColor(f *os.File) bool { return IsTerminal(f) }

var cols atomic.Int32

func querySize() {
	if out, err := stty("size"); err == nil {
		if f := strings.Fields(out); len(f) == 2 {
			if c, err := strconv.Atoi(f[1]); err == nil && c > 0 {
				cols.Store(int32(c))
				return
			}
		}
	}
	if c, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && c > 0 {
		cols.Store(int32(c))
		return
	}
	cols.Store(80)
}

func terminalWidth(_ *os.File) func() int {
	querySize()
	return func() int { return int(cols.Load()) }
}

// watchResize re-reads the terminal size on SIGWINCH.
func watchResize(onResize func()) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				querySize()
				onResize()
			case <-done:
				return
			}
		}
	}()
	return func() { signal.Stop(ch); close(done) }
}
