package main

import (
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"gochat/internal/protocol"
	"gochat/internal/tui"
)

// style wraps text in ANSI codes when colour is enabled.
type style struct{ on bool }

const reset = "\033[0m"

func (s style) wrap(code, text string) string {
	if !s.on || code == "" {
		return text
	}
	return code + text + reset
}

func (s style) dim(t string) string     { return s.wrap("\033[2m", t) }
func (s style) bold(t string) string    { return s.wrap("\033[1m", t) }
func (s style) italic(t string) string  { return s.wrap("\033[3m", t) }
func (s style) green(t string) string   { return s.wrap("\033[32m", t) }
func (s style) yellow(t string) string  { return s.wrap("\033[33m", t) }
func (s style) red(t string) string     { return s.wrap("\033[31m", t) }
func (s style) magenta(t string) string { return s.wrap("\033[35m", t) }
func (s style) cyan(t string) string    { return s.wrap("\033[36m", t) }
func (s style) highlight(t string) string {
	return s.wrap("\033[1;30;43m", t)
}

// Each username gets a stable colour so conversations are easy to follow.
var namePalette = []string{"39", "208", "170", "41", "220", "99", "45", "203", "112", "177", "214", "81"}

func (s style) name(n string) string {
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(n)))
	c := namePalette[int(h.Sum32())%len(namePalette)]
	return s.wrap("\033[1;38;5;"+c+"m", n)
}

const nameCol = 10

// padName right-pads a coloured name to a fixed column.
func (s style) paddedName(n string) string {
	pad := nameCol - tui.VisibleWidth(n)
	if pad < 1 {
		pad = 1
	}
	return s.name(n) + strings.Repeat(" ", pad)
}

func (s style) stamp(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return s.dim(t.Local().Format("15:04") + " │")
}

// line formats one chat line: "15:04 │ body".
func (s style) line(t time.Time, body string) string { return s.stamp(t) + " " + body }

func (s style) message(t time.Time, from, text string) string {
	return s.line(t, s.paddedName(from)+text)
}

func (s style) action(t time.Time, from, text string) string {
	return s.line(t, s.magenta("* ")+s.name(from)+" "+s.italic(text))
}

func (s style) private(t time.Time, from, to, text string) string {
	return s.line(t, s.magenta("✉ ")+s.name(from)+s.magenta(" → ")+s.name(to)+"  "+s.magenta(text))
}

func (s style) system(t time.Time, text string) string {
	return s.line(t, s.cyan("● "+text))
}

func (s style) errorf(format string, args ...any) string {
	return s.line(time.Now(), s.red("✖ "+fmt.Sprintf(format, args...)))
}

func (s style) online(n int) string {
	if n == 1 {
		return s.dim(" · 1 online")
	}
	return s.dim(fmt.Sprintf(" · %d online", n))
}

func (s style) event(ev protocol.Event) string {
	switch ev.Kind {
	case protocol.EventJoin:
		return s.line(ev.Time, s.green("→ ")+s.name(ev.From)+s.green(" joined the chat")+s.online(ev.Online))
	case protocol.EventLeave:
		if ev.Reason == protocol.ReasonQuit {
			return s.line(ev.Time, s.yellow("← ")+s.name(ev.From)+s.yellow(" left the chat")+s.online(ev.Online))
		}
		return s.line(ev.Time, s.red("✖ ")+s.name(ev.From)+s.red(" disconnected ("+ev.Reason+")")+s.online(ev.Online))
	case protocol.EventSystem:
		return s.system(ev.Time, ev.Text)
	case protocol.EventShutdown:
		return s.line(ev.Time, s.wrap("\033[1;31m", "⚡ "+ev.Text))
	}
	return ""
}

// bar draws a left-bar panel, used for the welcome screen and /users.
func (s style) bar(lines ...string) string {
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s.cyan("  ┃ ") + l)
	}
	return b.String()
}

func since(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm ago", int(d.Hours()), int(d.Minutes())%60)
	}
}

const logo = `   ___     ___ _         _
  / __|___/ __| |_  __ _| |_
 | (_ / _ \ (__| ' \/ _` + "`" + ` |  _|
  \___\___/\___|_||_\__,_|\__|`
