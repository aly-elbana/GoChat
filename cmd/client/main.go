// Command client is the GoChat terminal client.
//
//	go run ./cmd/client -server localhost:9090 -name alice
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"gochat/internal/client"
	"gochat/internal/protocol"
	"gochat/internal/tui"
)

func main() {
	serverAddr := flag.String("server", "localhost:9090", "server address (host:port)")
	name := flag.String("name", "", "username (asked interactively if empty)")
	noColor := flag.Bool("no-color", false, "disable colours")
	plain := flag.Bool("plain", false, "simple line mode (no interactive editor)")
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Println("gochat client", protocol.Version)
		return
	}

	a := &app{users: map[string]string{}}

	// Pick the terminal front-end: the interactive editor when we have a
	// real terminal, otherwise plain line input.
	var ed *tui.Editor
	if !*plain {
		if e, err := tui.New(""); err == nil {
			ed = e
		}
	}
	if ed != nil {
		a.io, a.tty = ed, true
		ed.Complete = a.complete
	} else {
		a.io = newPlainIO()
	}
	_, noColorEnv := os.LookupEnv("NO_COLOR")
	a.st = style{on: !*noColor && !noColorEnv && (ed != nil || tui.EnableColor(os.Stdout))}

	// Ctrl+C, kill, or a closed terminal: leave gracefully so the others see
	// "left the chat" rather than "lost connection".
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sig
		a.exit(0, a.st.dim("Goodbye 👋"), true)
	}()

	a.println(a.st.cyan(logo))
	a.println(a.st.dim("  concurrent chat over Go net/rpc · v" + protocol.Version))
	a.println("")

	a.println(a.st.dim("Connecting to " + *serverAddr + " …"))
	c, err := client.Dial(*serverAddr)
	if err != nil {
		a.exit(1, a.st.red("✖ Could not connect to "+*serverAddr+": "+err.Error()), false)
	}
	a.c = c

	reply := a.join(strings.TrimSpace(*name))
	a.me = reply.Username
	for _, u := range reply.Users {
		a.addUser(u.Name)
	}
	a.addUser(a.me)
	a.welcome(reply)
	a.io.SetPrompt(a.prompt())

	go a.receive()
	a.loop()
}

// join asks for a username until the server accepts one.
func (a *app) join(name string) *protocol.JoinReply {
	interactive := a.tty
	for {
		if name == "" {
			a.io.SetPrompt(a.st.bold("Choose a username") + a.st.dim(" ❯ "))
			line, err := a.io.ReadLine()
			if err != nil {
				a.exit(0, "", false)
			}
			name = strings.TrimSpace(line)
			if name == "" {
				continue
			}
		}
		reply, err := a.c.Join(name)
		if err == nil {
			a.io.SetPrompt("")
			return reply
		}
		switch {
		case errors.Is(err, client.ErrNameTaken):
			a.println(a.st.errorf("%q is already taken — pick another name", name))
		case errors.Is(err, client.ErrClosed):
			a.exit(1, a.st.red("✖ Lost connection to the server."), false)
		default:
			a.println(a.st.errorf("%v", err))
		}
		if !interactive && flag.Lookup("name").Value.String() != "" {
			a.exit(1, "", false) // non-interactive run with a bad -name
		}
		name = ""
	}
}
