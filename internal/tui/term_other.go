//go:build !unix && !windows

package tui

import (
	"errors"
	"os"
)

func enableRaw(*os.File) (func() error, error) {
	return nil, errors.New("interactive mode not supported on this platform")
}

// EnableColor reports whether ANSI codes can be used on f.
func EnableColor(*os.File) bool { return false }

func terminalWidth(*os.File) func() int { return func() int { return 80 } }

func watchResize(func()) func() { return func() {} }
