// Command server runs the GoChat server.
//
//	go run ./cmd/server -addr :9090
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gochat/internal/protocol"
	"gochat/internal/server"
	"gochat/internal/tui"
)

const banner = `
   ___     ___ _         _
  / __|___/ __| |_  __ _| |_
 | (_ / _ \ (__| ' \/ _` + "`" + ` |  _|
  \___\___/\___|_||_\__,_|\__|   server`

func main() {
	addr := flag.String("addr", ":9090", "address to listen on")
	name := flag.String("name", "GoChat", "server name shown to clients")
	motd := flag.String("motd", "Be kind. Have fun. 🐹", "message of the day")
	history := flag.Int("history", 20, "recent messages replayed to new users (0 to disable)")
	noColor := flag.Bool("no-color", false, "disable colours")
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Println("gochat server", protocol.Version)
		return
	}

	_, noColorEnv := os.LookupEnv("NO_COLOR")
	color := !*noColor && !noColorEnv && tui.EnableColor(os.Stdout)
	log := server.NewLogger(os.Stdout, color)

	hist := *history
	if hist <= 0 {
		hist = -1 // disabled
	}
	srv := server.New(server.Config{Name: *name, MOTD: *motd, HistorySize: hist, Logger: log})

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✖ cannot listen on %s: %v\n", *addr, err)
		os.Exit(1)
	}

	log.Raw(log.Paint(server.Cyan, banner) + "\n\n")
	log.Raw(log.Paint(server.Bold, "  "+*name+" v"+protocol.Version) + log.Paint(server.Dim, "  ·  net/rpc  ·  server-push callbacks") + "\n")
	port := ln.Addr().(*net.TCPAddr).Port
	log.Raw(log.Paint(server.Dim, "  listening on ") + ln.Addr().String() + "\n")
	for _, ip := range lanAddrs() {
		log.Raw(log.Paint(server.Dim, "  join from another machine: ") + log.Paint(server.Green, fmt.Sprintf("go run ./cmd/client -server %s:%d", ip, port)) + "\n")
	}
	log.Raw(log.Paint(server.Dim, "  press Ctrl+C to shut down gracefully") + "\n\n")

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		<-sig
		fmt.Println()
		log.Warn("shutting down — notifying clients (press Ctrl+C again to force)")
		go func() {
			<-sig
			log.Warn("forced exit")
			os.Exit(1)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Warn("shutdown: %v", err)
		}
		close(done)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, server.ErrServerClosed) {
		log.Warn("serve: %v", err)
		os.Exit(1)
	}
	<-done
	st := srv.Stats()
	log.Info("bye! uptime %s · %d joins · %d messages · peak %d online",
		st.Uptime.Round(time.Second), st.Joins, st.Messages, st.Peak)
}

// lanAddrs lists this machine's non-loopback IPv4 addresses.
func lanAddrs() []string {
	var out []string
	ifaces, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range ifaces {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		if s := ipn.IP.String(); !strings.HasPrefix(s, "169.254.") {
			out = append(out, s)
		}
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}
