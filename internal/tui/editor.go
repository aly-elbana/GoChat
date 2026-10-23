// Package tui is a tiny, dependency-free line editor for chat programs.
//
// The hard part of a terminal chat client is that messages arrive while the
// user is typing. Editor solves that: Println can be called from any
// goroutine at any time — it erases the input line, prints the message above
// it, and redraws the prompt with whatever the user had typed, cursor and all.
//
// Features: cursor movement, Home/End, word delete, history (↑/↓), Tab
// completion, Ctrl+L to clear. Long input scrolls horizontally instead of
// wrapping, which keeps redraws exact on every terminal.
package tui

import (
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// ErrInterrupted is returned by ReadLine when the user presses Ctrl+C in a
// terminal that delivers it as a key rather than a signal.
var ErrInterrupted = errors.New("interrupted")

// Completer is called on Tab. It returns the new line and cursor position,
// plus candidates to show when the completion is ambiguous. It must not call
// back into the Editor.
type Completer func(line string, pos int) (newLine string, newPos int, candidates []string)

// Editor is an interactive line editor bound to a terminal.
type Editor struct {
	in      io.Reader
	out     io.Writer
	restore func() error
	width   func() int
	stopRes func()

	// Complete, if set, is called on Tab.
	Complete Completer

	mu      sync.Mutex
	prompt  string
	pw      int // visible width of prompt
	line    []rune
	pos     int
	scroll  int
	shown   bool // prompt currently drawn
	hist    []string
	hidx    int
	draft   []rune
	pending []byte
	lastCR  bool
	closed  bool
}

// New puts the terminal attached to stdin into character mode and returns an
// editor drawing on stdout. It fails if stdin/stdout aren't a terminal that
// supports this; callers should then fall back to plain line input.
func New(prompt string) (*Editor, error) {
	if !IsTerminal(os.Stdin) || !IsTerminal(os.Stdout) {
		return nil, errors.New("not a terminal")
	}
	if !EnableColor(os.Stdout) {
		return nil, errors.New("terminal does not support ANSI escape codes")
	}
	restore, err := enableRaw(os.Stdin)
	if err != nil {
		return nil, err
	}
	e := newEditor(os.Stdin, os.Stdout, prompt)
	e.restore = restore
	e.width = terminalWidth(os.Stdout)
	e.stopRes = watchResize(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.shown && !e.closed {
			e.render()
		}
	})
	return e, nil
}

func newEditor(in io.Reader, out io.Writer, prompt string) *Editor {
	e := &Editor{in: in, out: out, width: func() int { return 80 }}
	e.setPrompt(prompt)
	return e
}

// SetPrompt changes the prompt (it may contain ANSI colour codes).
func (e *Editor) SetPrompt(p string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.setPrompt(p)
	if e.shown {
		e.render()
	}
}

func (e *Editor) setPrompt(p string) {
	e.prompt = p
	e.pw = VisibleWidth(p)
}

// Println prints s on its own line(s) above the input line. Safe to call
// concurrently with ReadLine.
func (e *Editor) Println(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		io.WriteString(e.out, s+"\n")
		return
	}
	var b strings.Builder
	b.WriteString("\r\033[K")
	b.WriteString(strings.ReplaceAll(s, "\n", "\033[K\n"))
	b.WriteString("\033[K\n")
	io.WriteString(e.out, b.String())
	if e.shown {
		e.render()
	}
}

// ClearScreen wipes the terminal and redraws the prompt.
func (e *Editor) ClearScreen() {
	e.mu.Lock()
	defer e.mu.Unlock()
	io.WriteString(e.out, "\033[H\033[2J")
	if e.shown {
		e.render()
	}
}

// Close restores the terminal. The editor keeps working as a plain printer.
func (e *Editor) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	if e.shown {
		io.WriteString(e.out, "\r\033[K")
		e.shown = false
	}
	if e.stopRes != nil {
		e.stopRes()
	}
	if e.restore != nil {
		return e.restore()
	}
	return nil
}

// ReadLine waits for the user to press Enter and returns the line.
// It returns io.EOF on Ctrl+D (on an empty line) or end of input.
func (e *Editor) ReadLine() (string, error) {
	e.mu.Lock()
	e.shown = true
	e.render()
	e.mu.Unlock()

	buf := make([]byte, 256)
	for {
		e.mu.Lock()
		line, done, err := e.process()
		e.mu.Unlock()
		if done {
			return line, err
		}
		n, rerr := e.in.Read(buf)
		if n > 0 {
			e.mu.Lock()
			e.pending = append(e.pending, buf[:n]...)
			e.mu.Unlock()
		}
		if rerr != nil && n == 0 {
			return "", io.EOF
		}
	}
}

type key int

const (
	keyNone key = iota
	keyUp
	keyDown
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyDelete
	keyWordLeft
	keyWordRight
)

// process consumes pending input. mu must be held.
func (e *Editor) process() (string, bool, error) {
	for len(e.pending) > 0 {
		b := e.pending[0]
		switch {
		case b == 0x1b:
			n, k := parseEscape(e.pending)
			if n == 0 {
				return "", false, nil // wait for the rest of the sequence
			}
			e.pending = e.pending[n:]
			e.lastCR = false
			e.handleKey(k)

		case b == '\r' || b == '\n':
			e.pending = e.pending[1:]
			if b == '\n' && e.lastCR { // CR LF from a paste
				e.lastCR = false
				continue
			}
			e.lastCR = b == '\r'
			return e.submit(), true, nil

		case b < 0x20 || b == 0x7f:
			e.pending = e.pending[1:]
			e.lastCR = false
			switch b {
			case 0x03: // Ctrl+C delivered as a key
				e.clearInput()
				return "", true, ErrInterrupted
			case 0x04: // Ctrl+D
				if len(e.line) == 0 {
					return "", true, io.EOF
				}
				e.deleteAt(e.pos)
			case 0x7f, 0x08:
				if e.pos > 0 {
					e.pos--
					e.deleteAt(e.pos)
				}
			case 0x01:
				e.pos = 0
			case 0x05:
				e.pos = len(e.line)
			case 0x02:
				e.handleKey(keyLeft)
			case 0x06:
				e.handleKey(keyRight)
			case 0x0b: // Ctrl+K
				e.line = e.line[:e.pos]
			case 0x15: // Ctrl+U
				e.line = append([]rune{}, e.line[e.pos:]...)
				e.pos = 0
			case 0x17: // Ctrl+W
				start := wordStart(e.line, e.pos)
				e.line = append(e.line[:start], e.line[e.pos:]...)
				e.pos = start
			case 0x0c: // Ctrl+L
				io.WriteString(e.out, "\033[H\033[2J")
			case 0x10:
				e.handleKey(keyUp)
			case 0x0e:
				e.handleKey(keyDown)
			case '\t':
				e.complete()
			}
			e.render()

		default:
			if !utf8.FullRune(e.pending) {
				return "", false, nil
			}
			r, size := utf8.DecodeRune(e.pending)
			e.pending = e.pending[size:]
			e.lastCR = false
			if r == utf8.RuneError && size == 1 {
				continue
			}
			e.line = append(e.line, 0)
			copy(e.line[e.pos+1:], e.line[e.pos:])
			e.line[e.pos] = r
			e.pos++
			e.render()
		}
	}
	return "", false, nil
}

func (e *Editor) handleKey(k key) {
	switch k {
	case keyLeft:
		if e.pos > 0 {
			e.pos--
		}
	case keyRight:
		if e.pos < len(e.line) {
			e.pos++
		}
	case keyHome:
		e.pos = 0
	case keyEnd:
		e.pos = len(e.line)
	case keyDelete:
		e.deleteAt(e.pos)
	case keyWordLeft:
		e.pos = wordStart(e.line, e.pos)
	case keyWordRight:
		i := e.pos
		for i < len(e.line) && unicode.IsSpace(e.line[i]) {
			i++
		}
		for i < len(e.line) && !unicode.IsSpace(e.line[i]) {
			i++
		}
		e.pos = i
	case keyUp:
		if e.hidx > 0 {
			if e.hidx == len(e.hist) {
				e.draft = append([]rune{}, e.line...)
			}
			e.hidx--
			e.line = []rune(e.hist[e.hidx])
			e.pos = len(e.line)
		}
	case keyDown:
		if e.hidx < len(e.hist) {
			e.hidx++
			if e.hidx == len(e.hist) {
				e.line = e.draft
			} else {
				e.line = []rune(e.hist[e.hidx])
			}
			e.pos = len(e.line)
		}
	}
	e.render()
}

func (e *Editor) deleteAt(i int) {
	if i >= 0 && i < len(e.line) {
		e.line = append(e.line[:i], e.line[i+1:]...)
	}
}

func wordStart(line []rune, pos int) int {
	i := pos
	for i > 0 && unicode.IsSpace(line[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(line[i-1]) {
		i--
	}
	return i
}

func (e *Editor) complete() {
	if e.Complete == nil {
		return
	}
	line, pos, cands := e.Complete(string(e.line), runeToByte(e.line, e.pos))
	e.line = []rune(line)
	e.pos = utf8.RuneCountInString(line[:min(pos, len(line))])
	if len(cands) > 1 {
		io.WriteString(e.out, "\r\033[K\033[2m"+strings.Join(cands, "  ")+"\033[0m\033[K\n")
	}
}

func runeToByte(line []rune, pos int) int { return len(string(line[:pos])) }

func (e *Editor) submit() string {
	s := string(e.line)
	if strings.TrimSpace(s) != "" && (len(e.hist) == 0 || e.hist[len(e.hist)-1] != s) {
		e.hist = append(e.hist, s)
		if len(e.hist) > 200 {
			e.hist = e.hist[1:]
		}
	}
	e.hidx = len(e.hist)
	e.draft = nil
	e.clearInput()
	return s
}

func (e *Editor) clearInput() {
	e.line = nil
	e.pos = 0
	e.scroll = 0
	e.render()
}

// render redraws the prompt and the visible window of the input. mu held.
func (e *Editor) render() {
	w := e.width()
	if w <= 0 {
		w = 80
	}
	avail := w - e.pw - 1
	if avail < 8 {
		avail = 8
	}
	if e.pos < e.scroll {
		e.scroll = e.pos
	}
	for e.scroll < e.pos && widthOf(e.line[e.scroll:e.pos]) > avail {
		e.scroll++
	}
	end, vw := e.scroll, 0
	for end < len(e.line) {
		rw := RuneWidth(e.line[end])
		if vw+rw > avail {
			break
		}
		vw += rw
		end++
	}
	var b strings.Builder
	b.WriteString("\r\033[K")
	b.WriteString(e.prompt)
	b.WriteString(string(e.line[e.scroll:end]))
	b.WriteString("\r")
	if col := e.pw + widthOf(e.line[e.scroll:e.pos]); col > 0 {
		b.WriteString("\033[" + strconv.Itoa(col) + "C")
	}
	io.WriteString(e.out, b.String())
}

// parseEscape decodes an escape sequence at the start of p. It returns the
// number of bytes consumed (0 = incomplete, wait for more) and the key.
func parseEscape(p []byte) (int, key) {
	if len(p) < 2 {
		return 0, keyNone
	}
	switch p[1] {
	case '[':
		for i := 2; i < len(p); i++ {
			c := p[i]
			if c >= 0x40 && c <= 0x7e {
				return i + 1, csiKey(string(p[2:i]), c)
			}
			if c < 0x20 || c > 0x3f {
				return i, keyNone // malformed: drop it
			}
		}
		if len(p) > 16 {
			return len(p), keyNone
		}
		return 0, keyNone
	case 'O':
		if len(p) < 3 {
			return 0, keyNone
		}
		return 3, csiKey("", p[2])
	default:
		return 1, keyNone // lone ESC / Alt+key: ignore the ESC
	}
}

func csiKey(params string, final byte) key {
	ctrl := strings.HasSuffix(params, ";5") || strings.HasSuffix(params, ";3")
	switch final {
	case 'A':
		return keyUp
	case 'B':
		return keyDown
	case 'C':
		if ctrl {
			return keyWordRight
		}
		return keyRight
	case 'D':
		if ctrl {
			return keyWordLeft
		}
		return keyLeft
	case 'H':
		return keyHome
	case 'F':
		return keyEnd
	case '~':
		switch params {
		case "1", "7":
			return keyHome
		case "4", "8":
			return keyEnd
		case "3":
			return keyDelete
		}
	}
	return keyNone
}

// VisibleWidth is the number of terminal columns s occupies, ignoring ANSI
// escape sequences.
func VisibleWidth(s string) int {
	w := 0
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if r >= 0x40 && r <= 0x7e && r != '[' {
				inEsc = false
			}
		case r == 0x1b:
			inEsc = true
		default:
			w += RuneWidth(r)
		}
	}
	return w
}

func widthOf(rs []rune) int {
	w := 0
	for _, r := range rs {
		w += RuneWidth(r)
	}
	return w
}

// RuneWidth approximates how many columns r takes: 0 for combining marks,
// 2 for East Asian wide characters and most emoji, 1 otherwise.
func RuneWidth(r rune) int {
	switch {
	case r == 0 || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200d:
		return 0
	case r >= 0x1100 && r <= 0x115f,
		r >= 0x2e80 && r <= 0xa4cf && r != 0x303f,
		r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff,
		r >= 0xfe30 && r <= 0xfe4f,
		r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f300 && r <= 0x1f64f,
		r >= 0x1f900 && r <= 0x1f9ff,
		r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}
