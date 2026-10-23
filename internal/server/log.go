package server

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Logger prints the server's colourful activity feed. It is safe for
// concurrent use. A nil *Logger discards everything.
type Logger struct {
	mu    sync.Mutex
	w     io.Writer
	color bool
}

// NewLogger returns a Logger that writes to w.
func NewLogger(w io.Writer, color bool) *Logger { return &Logger{w: w, color: color} }

const (
	cReset  = "\033[0m"
	cDim    = "\033[2m"
	cBold   = "\033[1m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cBlue   = "\033[34m"
	cPurple = "\033[35m"
	cCyan   = "\033[36m"
)

func (l *Logger) paint(code, s string) string {
	if l == nil || !l.color {
		return s
	}
	return code + s + cReset
}

func (l *Logger) line(icon, tag, color, format string, args ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := time.Now().Format("15:04:05")
	fmt.Fprintf(l.w, "%s %s %s %s\n",
		l.paint(cDim, ts),
		l.paint(color, icon),
		l.paint(color+cBold, fmt.Sprintf("%-6s", tag)),
		fmt.Sprintf(format, args...))
}

func (l *Logger) Info(format string, args ...any)  { l.line("●", "info", cBlue, format, args...) }
func (l *Logger) Join(format string, args ...any)  { l.line("▲", "join", cGreen, format, args...) }
func (l *Logger) Leave(format string, args ...any) { l.line("▼", "leave", cYellow, format, args...) }
func (l *Logger) Drop(format string, args ...any)  { l.line("✖", "drop", cRed, format, args...) }
func (l *Logger) Msg(format string, args ...any)   { l.line("✉", "msg", cCyan, format, args...) }
func (l *Logger) DM(format string, args ...any)    { l.line("✉", "dm", cPurple, format, args...) }
func (l *Logger) Warn(format string, args ...any)  { l.line("⚠", "warn", cYellow, format, args...) }

// Raw writes s as-is.
func (l *Logger) Raw(s string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	io.WriteString(l.w, s)
}

// Paint exposes the colour helper for banners.
func (l *Logger) Paint(code, s string) string { return l.paint(code, s) }

// Exported colour codes for use by cmd/server.
const (
	Bold   = cBold
	Dim    = cDim
	Cyan   = cCyan
	Green  = cGreen
	Yellow = cYellow
)
