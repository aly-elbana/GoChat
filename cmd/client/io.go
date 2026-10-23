package main

import (
	"bufio"
	"io"
	"os"
	"sync"

	"gochat/internal/tui"
)

// lineIO is what the chat app needs from the terminal.
type lineIO interface {
	ReadLine() (string, error)
	Println(string)
	SetPrompt(string)
	ClearScreen()
	Close() error
}

var _ lineIO = (*tui.Editor)(nil)

// plainIO is the fallback when stdin isn't an interactive terminal (pipes,
// CI, unsupported consoles) or -plain is given: line-buffered input and
// mutex-protected output.
type plainIO struct {
	mu sync.Mutex
	sc *bufio.Scanner
	w  io.Writer
}

func newPlainIO() *plainIO {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	return &plainIO{sc: sc, w: os.Stdout}
}

func (p *plainIO) ReadLine() (string, error) {
	if !p.sc.Scan() {
		if err := p.sc.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	return p.sc.Text(), nil
}

func (p *plainIO) Println(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	io.WriteString(p.w, s+"\n")
}

func (p *plainIO) SetPrompt(string) {}
func (p *plainIO) ClearScreen()     {}
func (p *plainIO) Close() error     { return nil }
