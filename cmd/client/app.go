package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"gochat/internal/client"
	"gochat/internal/protocol"
	"gochat/internal/tui"
)

type app struct {
	c   *client.Client
	io  lineIO
	st  style
	me  string
	tty bool

	mu    sync.Mutex
	users map[string]string // lower-case key -> display name (for Tab)

	quitting      atomic.Bool
	serverClosing atomic.Bool
	exitOnce      sync.Once
}

// exit tears everything down exactly once: optionally says goodbye to the
// server, restores the terminal, prints a final line and exits.
func (a *app) exit(code int, msg string, leave bool) {
	a.exitOnce.Do(func() {
		a.quitting.Store(true)
		if a.c != nil {
			if leave {
				a.c.Leave()
			} else {
				a.c.Close()
			}
		}
		a.io.Close()
		if msg != "" {
			fmt.Println(msg)
		}
		os.Exit(code)
	})
	select {} // another goroutine is already exiting
}

func (a *app) println(s string) { a.io.Println(s) }

func (a *app) prompt() string {
	return a.st.name(a.me) + a.st.dim(" ❯ ")
}

// --- incoming events -------------------------------------------------------

func (a *app) receive() {
	replayed := 0
	for ev := range a.c.Events() {
		switch ev.Kind {
		case protocol.EventMessage, protocol.EventAction:
			if ev.Replay {
				if replayed == 0 {
					a.println(a.st.dim("  ── recent messages ──"))
				}
				replayed++
				a.println(a.st.dim(a.plainLine(ev)))
				continue
			}
			a.println(a.renderMessage(ev))
		case protocol.EventPrivate:
			a.println(a.st.private(ev.Time, ev.From, "you", ev.Text))
			a.bell()
		case protocol.EventReady:
			if replayed > 0 {
				a.println(a.st.dim("  ── you're live ──"))
			}
		case protocol.EventJoin:
			a.addUser(ev.From)
			a.println(a.st.event(ev))
		case protocol.EventLeave:
			a.removeUser(ev.From)
			a.println(a.st.event(ev))
		case protocol.EventShutdown:
			a.serverClosing.Store(true)
			a.println(a.st.event(ev))
		default:
			if s := a.st.event(ev); s != "" {
				a.println(s)
			}
		}
	}
	// The push channel is gone.
	switch {
	case a.quitting.Load():
		return
	case a.serverClosing.Load():
		a.exit(0, a.st.yellow("The server closed the chat. Goodbye!"), false)
	default:
		a.exit(1, a.st.red("✖ Lost connection to the server."), false)
	}
}

func (a *app) renderMessage(ev protocol.Event) string {
	mention := a.mentionsMe(ev.Text)
	text := ev.Text
	if mention {
		text = a.st.highlight(text)
		a.bell()
	}
	if ev.Kind == protocol.EventAction {
		return a.st.action(ev.Time, ev.From, text)
	}
	return a.st.message(ev.Time, ev.From, text)
}

func (a *app) plainLine(ev protocol.Event) string {
	plain := style{}
	if ev.Kind == protocol.EventAction {
		return plain.action(ev.Time, ev.From, ev.Text)
	}
	return plain.message(ev.Time, ev.From, ev.Text)
}

func (a *app) mentionsMe(text string) bool {
	return strings.Contains(strings.ToLower(text), "@"+strings.ToLower(a.me))
}

func (a *app) bell() {
	if a.tty {
		os.Stdout.WriteString("\a")
	}
}

func (a *app) addUser(n string) {
	a.mu.Lock()
	a.users[strings.ToLower(n)] = n
	a.mu.Unlock()
}

func (a *app) removeUser(n string) {
	a.mu.Lock()
	delete(a.users, strings.ToLower(n))
	a.mu.Unlock()
}

// --- outgoing --------------------------------------------------------------

func (a *app) loop() {
	for {
		line, err := a.io.ReadLine()
		if err != nil {
			a.exit(0, a.st.dim("Goodbye 👋"), true)
		}
		a.handle(line)
	}
}

func (a *app) handle(line string) {
	text := strings.TrimSpace(line)
	if text == "" {
		return
	}
	if strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "//") {
		a.command(text)
		return
	}
	text = strings.TrimPrefix(text, "/") // "//foo" sends "/foo"
	r, err := a.c.Send(text)
	if err != nil {
		a.sendError(err)
		return
	}
	// The server never echoes our own messages, so show it locally.
	out := a.st.message(r.Time, a.me, r.Text)
	if r.Recipients == 0 {
		out += a.st.dim("  (nobody else is here yet)")
	}
	a.println(out)
}

func (a *app) sendError(err error) {
	if errors.Is(err, client.ErrClosed) {
		return // receive() will report the lost connection
	}
	a.println(a.st.errorf("%v", err))
}

type command struct {
	names []string
	args  string
	help  string
}

var commands = []command{
	{[]string{"/help", "/h", "/?"}, "", "show this help"},
	{[]string{"/users", "/who", "/list"}, "", "list who is online"},
	{[]string{"/msg", "/w", "/dm"}, "<user> <text>", "send a private message"},
	{[]string{"/me"}, "<action>", "describe an action, e.g. /me waves"},
	{[]string{"/clear"}, "", "clear the screen"},
	{[]string{"/quit", "/exit", "/q"}, "", "leave the chat (or press Ctrl+C)"},
}

func (a *app) command(text string) {
	name, rest, _ := strings.Cut(text, " ")
	rest = strings.TrimSpace(rest)
	switch strings.ToLower(name) {
	case "/help", "/h", "/?":
		a.help()
	case "/users", "/who", "/list":
		a.listUsers()
	case "/msg", "/w", "/dm", "/whisper":
		to, msg, _ := strings.Cut(rest, " ")
		to = strings.TrimPrefix(to, "@")
		if to == "" || strings.TrimSpace(msg) == "" {
			a.println(a.st.errorf("usage: /msg <user> <text>"))
			return
		}
		r, err := a.c.Whisper(to, msg)
		if err != nil {
			a.sendError(err)
			return
		}
		a.println(a.st.private(r.Time, "you", r.To, r.Text))
	case "/me":
		if rest == "" {
			a.println(a.st.errorf("usage: /me <action>"))
			return
		}
		r, err := a.c.Action(rest)
		if err != nil {
			a.sendError(err)
			return
		}
		a.println(a.st.action(r.Time, a.me, r.Text))
	case "/clear", "/cls":
		a.io.ClearScreen()
	case "/quit", "/exit", "/q", "/leave":
		a.exit(0, a.st.dim("Goodbye 👋"), true)
	default:
		a.println(a.st.errorf("unknown command %s — try /help", name))
	}
}

func (a *app) help() {
	lines := []string{a.st.bold("Commands")}
	for _, c := range commands {
		usage := c.names[0]
		if c.args != "" {
			usage += " " + c.args
		}
		alias := ""
		if len(c.names) > 1 {
			alias = a.st.dim("  (" + strings.Join(c.names[1:], ", ") + ")")
		}
		lines = append(lines, fmt.Sprintf("%-22s %s%s", usage, c.help, alias))
	}
	lines = append(lines,
		a.st.dim("Tab completes commands and names · ↑/↓ history · @name mentions ring a bell"),
		a.st.dim("Start a message with // to send text beginning with a slash"))
	a.println(a.st.bar(lines...))
}

func (a *app) listUsers() {
	users, err := a.c.Users()
	if err != nil {
		a.sendError(err)
		return
	}
	lines := []string{a.st.bold(fmt.Sprintf("Online (%d)", len(users)))}
	for _, u := range users {
		tag := ""
		if strings.EqualFold(u.Name, a.me) {
			tag = a.st.dim(" (you)")
		}
		label := a.st.name(u.Name) + tag
		pad := strings.Repeat(" ", max(1, 18-tui.VisibleWidth(label)))
		lines = append(lines, a.st.green("● ")+label+pad+a.st.dim("joined "+since(u.JoinedAt)))
	}
	a.println(a.st.bar(lines...))
	// Refresh the completion list while we're at it.
	a.mu.Lock()
	a.users = map[string]string{}
	for _, u := range users {
		a.users[strings.ToLower(u.Name)] = u.Name
	}
	a.mu.Unlock()
}

// complete implements Tab completion for commands and usernames.
func (a *app) complete(line string, pos int) (string, int, []string) {
	head, tail := line[:pos], line[pos:]
	start := strings.LastIndexAny(head, " \t") + 1
	word := head[start:]
	if word == "" {
		return line, pos, nil
	}

	var pool []string
	prefix := ""
	switch {
	case start == 0 && strings.HasPrefix(word, "/"):
		for _, c := range commands {
			pool = append(pool, c.names[0])
		}
	default:
		if strings.HasPrefix(word, "@") {
			prefix, word = "@", word[1:]
		}
		a.mu.Lock()
		for _, n := range a.users {
			if !strings.EqualFold(n, a.me) {
				pool = append(pool, n)
			}
		}
		a.mu.Unlock()
	}
	var matches []string
	for _, p := range pool {
		if strings.HasPrefix(strings.ToLower(p), strings.ToLower(word)) {
			matches = append(matches, p)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 0:
		return line, pos, nil
	case 1:
		repl := prefix + matches[0] + " "
		return head[:start] + repl + tail, start + len(repl), nil
	}
	common := matches[0]
	for _, m := range matches[1:] {
		for !strings.HasPrefix(strings.ToLower(m), strings.ToLower(common)) {
			common = common[:len(common)-1]
		}
	}
	if len(common) > len(word) {
		repl := prefix + common
		return head[:start] + repl + tail, start + len(repl), matches
	}
	return line, pos, matches
}

// welcome prints the panel shown right after joining.
func (a *app) welcome(r *protocol.JoinReply) {
	names := []string{a.st.name(a.me) + a.st.dim(" (you)")}
	for _, u := range r.Users {
		names = append(names, a.st.name(u.Name))
	}
	lines := []string{
		a.st.bold("Welcome to "+r.ServerName+", ") + a.st.name(a.me) + a.st.bold("!"),
	}
	if r.MOTD != "" {
		lines = append(lines, a.st.italic(r.MOTD))
	}
	lines = append(lines,
		fmt.Sprintf("Online now (%d): %s", len(names), strings.Join(names, ", ")),
		a.st.dim("Type a message and press Enter · /help for commands · Ctrl+C to quit"))
	a.println(a.st.bar(lines...))
	a.println("")
}
