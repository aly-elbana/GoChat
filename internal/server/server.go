// Package server implements the GoChat server: a registry of connected users
// reachable over net/rpc, which pushes messages and notifications back to the
// clients through RPC callbacks.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gochat/internal/protocol"
)

// Config tunes the server. Zero values are replaced by sensible defaults.
type Config struct {
	Name          string        // shown to clients when they join
	MOTD          string        // message of the day
	HistorySize   int           // recent messages replayed to new users (0 = 20, <0 = off)
	OutboxSize    int           // per-user queue before a client counts as too slow
	PingInterval  time.Duration // heartbeat period on the push channel
	CallTimeout   time.Duration // max time a client may take to accept a callback
	AttachTimeout time.Duration // max time between Join and opening the push channel
	Logger        *Logger       // nil = silent
}

func (c Config) withDefaults() Config {
	if c.Name == "" {
		c.Name = "GoChat"
	}
	switch {
	case c.HistorySize == 0:
		c.HistorySize = 20
	case c.HistorySize < 0:
		c.HistorySize = 0
	}
	if c.OutboxSize <= 0 {
		c.OutboxSize = 1024
	}
	if c.OutboxSize < c.HistorySize+16 {
		c.OutboxSize = c.HistorySize + 16
	}
	if c.PingInterval <= 0 {
		c.PingInterval = 5 * time.Second
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = 5 * time.Second
	}
	if c.AttachTimeout <= 0 {
		c.AttachTimeout = 10 * time.Second
	}
	return c
}

// ErrServerClosed is returned by Serve after Shutdown.
var ErrServerClosed = errors.New("server closed")

// Server accepts client connections and runs the chat.
type Server struct {
	cfg     Config
	log     *Logger
	hub     *hub
	started time.Time

	mu      sync.Mutex
	ln      net.Listener
	conns   map[net.Conn]struct{}
	closing bool
	connWG  sync.WaitGroup
}

// New creates a server.
func New(cfg Config) *Server {
	cfg = cfg.withDefaults()
	return &Server{
		cfg:   cfg,
		log:   cfg.Logger,
		hub:   newHub(cfg, cfg.Logger),
		conns: make(map[net.Conn]struct{}),
	}
}

// Serve accepts connections on ln until Shutdown is called.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return ErrServerClosed
	}
	s.ln = ln
	s.started = time.Now()
	s.mu.Unlock()

	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing {
				return ErrServerClosed
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				backoff = min(max(backoff*2, 5*time.Millisecond), time.Second)
				time.Sleep(backoff)
				continue
			}
			return err
		}
		backoff = 0
		if !s.track(conn) {
			conn.Close()
			continue
		}
		s.connWG.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// handleConn reads the handshake line and dispatches to the right channel.
func (s *Server) handleConn(conn net.Conn) {
	defer s.connWG.Done()
	defer s.untrack(conn)
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := protocol.ReadLine(conn, 128)
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		return
	}
	switch {
	case line == protocol.HelloControl:
		s.serveControl(conn)
	case strings.HasPrefix(line, protocol.HelloPush):
		s.servePush(conn, strings.TrimPrefix(line, protocol.HelloPush))
	default:
		protocol.WriteLine(conn, protocol.ReplyErr+"unknown handshake")
	}
}

// serveControl runs a private RPC server for this one connection. Binding the
// service instance to the connection means the server always knows which user
// is calling (no spoofing by name), and when ServeConn returns we know the
// client went away — gracefully or not.
func (s *Server) serveControl(conn net.Conn) {
	if err := protocol.WriteLine(conn, protocol.ReplyOK); err != nil {
		return
	}
	svc := &ChatService{hub: s.hub, cfg: s.cfg, conn: conn, remote: conn.RemoteAddr().String()}
	rs := rpc.NewServer()
	if err := rs.RegisterName(protocol.ServerService, svc); err != nil {
		s.log.Warn("register service: %v", err)
		return
	}
	rs.ServeConn(conn) // blocks until the connection closes
	svc.disconnected()
}

// servePush attaches the callback channel to a joined user and then runs
// that user's writer until they leave.
func (s *Server) servePush(conn net.Conn, token string) {
	sess, err := s.hub.attach(token, conn)
	if err != nil {
		protocol.WriteLine(conn, protocol.ReplyErr+err.Error())
		return
	}
	if err := protocol.WriteLine(conn, protocol.ReplyOK); err != nil {
		s.hub.remove(sess, protocol.ReasonLost)
	}
	s.hub.runWriter(sess)
}

// Shutdown notifies every client, lets their queues drain (bounded by ctx),
// and closes all connections. It is safe to call more than once.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	if s.ln != nil {
		s.ln.Close()
	}
	s.mu.Unlock()

	s.hub.shutdown()

	flushed := make(chan struct{})
	go func() { s.hub.writers.Wait(); close(flushed) }()
	var err error
	select {
	case <-flushed:
	case <-ctx.Done():
		err = ctx.Err()
	}

	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.connWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	return err
}

// Stats is a snapshot of server activity.
type Stats struct {
	Online, Joins, Messages, Peak int
	Uptime                        time.Duration
}

// Stats returns current counters.
func (s *Server) Stats() Stats {
	o, j, m, p := s.hub.stats()
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	return Stats{Online: o, Joins: j, Messages: m, Peak: p, Uptime: time.Since(started)}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func preview(text string) string {
	const max = 48
	if utf8.RuneCountInString(text) <= max {
		return fmt.Sprintf("%q", text)
	}
	r := []rune(text)
	return fmt.Sprintf("%q…", string(r[:max]))
}
