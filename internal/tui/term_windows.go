//go:build windows

package tui

import (
	"os"
	"syscall"
	"unsafe"
)

// On Windows we talk to the console API directly: line input and echo are
// switched off, processed input stays on (so Ctrl+C is still a signal), and
// virtual-terminal input/output is enabled so arrow keys arrive as ANSI
// sequences and colour codes render (Windows 10+ / Windows Terminal).

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

const (
	enableProcessedInput            = 0x0001
	enableLineInput                 = 0x0002
	enableEchoInput                 = 0x0004
	enableVirtualTerminalInput      = 0x0200
	enableVirtualTerminalProcessing = 0x0004
)

func setConsoleMode(h syscall.Handle, mode uint32) error {
	r, _, err := procSetConsoleMode.Call(uintptr(h), uintptr(mode))
	if r == 0 {
		return err
	}
	return nil
}

func enableRaw(in *os.File) (func() error, error) {
	h := syscall.Handle(in.Fd())
	var st uint32
	if err := syscall.GetConsoleMode(h, &st); err != nil {
		return nil, err
	}
	raw := st &^ (enableEchoInput | enableLineInput)
	raw |= enableProcessedInput | enableVirtualTerminalInput
	if err := setConsoleMode(h, raw); err != nil {
		return nil, err
	}
	return func() error { return setConsoleMode(h, st) }, nil
}

// EnableColor turns on ANSI processing for the console behind f.
func EnableColor(f *os.File) bool {
	h := syscall.Handle(f.Fd())
	var st uint32
	if err := syscall.GetConsoleMode(h, &st); err != nil {
		return false
	}
	if st&enableVirtualTerminalProcessing != 0 {
		return true
	}
	return setConsoleMode(h, st|enableVirtualTerminalProcessing) == nil
}

type coord struct{ x, y int16 }
type smallRect struct{ left, top, right, bottom int16 }
type screenBufferInfo struct {
	size       coord
	cursor     coord
	attributes uint16
	window     smallRect
	maxSize    coord
}

func terminalWidth(out *os.File) func() int {
	h := out.Fd()
	return func() int {
		var info screenBufferInfo
		r, _, _ := procGetConsoleScreenBufferInfo.Call(h, uintptr(unsafe.Pointer(&info)))
		if r == 0 {
			return 80
		}
		return int(info.window.right-info.window.left) + 1
	}
}

// Windows has no SIGWINCH; the width is queried on every redraw instead.
func watchResize(func()) func() { return func() {} }
