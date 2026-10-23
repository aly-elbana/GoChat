package tui

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
)

func readAll(t *testing.T, input string) []string {
	t.Helper()
	e := newEditor(strings.NewReader(input), io.Discard, "> ")
	var lines []string
	for {
		l, err := e.ReadLine()
		if err != nil {
			return lines
		}
		lines = append(lines, l)
	}
}

func TestEditing(t *testing.T) {
	cases := map[string]struct {
		in   string
		want []string
	}{
		"plain":            {"hello\r", []string{"hello"}},
		"backspace":        {"helo\x7f\x7fllo\r", []string{"hello"}},
		"arrows insert":    {"hllo\x1b[D\x1b[D\x1b[De\r", []string{"hello"}},
		"home end":         {"world\x1b[Hhello \x1b[F!\r", []string{"hello world!"}},
		"ctrl-w":           {"hello there\x17world\r", []string{"hello world"}},
		"ctrl-u":           {"garbage\x15ok\r", []string{"ok"}},
		"delete key":       {"abc\x1b[D\x1b[D\x1b[3~\r", []string{"ac"}},
		"history":          {"first\rsecond\r\x1b[A\x1b[A\r", []string{"first", "second", "first"}},
		"crlf paste":       {"a\r\nb\r\n", []string{"a", "b"}},
		"utf8":             {"héllo 👋\r", []string{"héllo 👋"}},
		"lone escape":      {"\x1bab\r", []string{"ab"}},
		"split utf8 bytes": {"\xc3\xa9\r", []string{"é"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := readAll(t, tc.in)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCtrlDOnEmptyLineIsEOF(t *testing.T) {
	e := newEditor(strings.NewReader("\x04"), io.Discard, "> ")
	if _, err := e.ReadLine(); err != io.EOF {
		t.Fatalf("err = %v, want EOF", err)
	}
}

func TestPrintlnRedrawsInput(t *testing.T) {
	out := &syncBuf{}
	pr, pw := io.Pipe()
	e := newEditor(pr, out, "> ")
	done := make(chan string)
	go func() { l, _ := e.ReadLine(); done <- l }()
	pw.Write([]byte("draft"))
	// Wait until the editor has consumed the keystrokes.
	for i := 0; i < 1000; i++ {
		e.mu.Lock()
		n := len(e.line)
		e.mu.Unlock()
		if n == 5 {
			break
		}
	}
	out.Reset()
	e.Println("incoming message")
	got := out.String()
	if !strings.Contains(got, "incoming message") || !strings.HasSuffix(strings.Split(got, "\n")[1], "\r\x1b[7C") || !strings.Contains(got, "> draft") {
		t.Fatalf("redraw output = %q", got)
	}
	pw.Write([]byte("\r"))
	if l := <-done; l != "draft" {
		t.Fatalf("line = %q", l)
	}
}

func TestTabCompletion(t *testing.T) {
	e := newEditor(strings.NewReader("/msg bo\tyo\r"), io.Discard, "> ")
	e.Complete = func(line string, pos int) (string, int, []string) {
		if line == "/msg bo" {
			return "/msg bob ", 9, nil
		}
		return line, pos, nil
	}
	if l, _ := e.ReadLine(); l != "/msg bob yo" {
		t.Fatalf("got %q", l)
	}
}

func TestVisibleWidth(t *testing.T) {
	if w := VisibleWidth("\x1b[1;38;5;81malice\x1b[0m ❯ "); w != 8 {
		t.Fatalf("width = %d, want 8", w)
	}
	if w := VisibleWidth("日本"); w != 4 {
		t.Fatalf("CJK width = %d, want 4", w)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
