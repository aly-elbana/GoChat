// Package client is the GoChat client library: it speaks the control channel
// (client -> server RPC) and serves the push channel (server -> client RPC
// callbacks). It has no UI; cmd/client builds the terminal app on top of it,
// and the integration tests drive it directly.
package client

import (
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gochat/internal/protocol"
)

// Errors returned by Join.
var (
	ErrNameTaken = errors.New(protocol.ErrNameTaken)
	ErrClosed    = errors.New("connection closed")
)

const callTimeout = 5 * time.Second

// Client is one connection to a GoChat server.
type Client struct {
	addr     string
	ctrlConn net.Conn
	ctrl     *rpc.Client

	mu       sync.Mutex
	pushConn net.Conn
	name     string

	events    chan protocol.Event
	lastSeen  atomic.Int64 // unix nanos of the last callback from the server
	leaving   atomic.Bool
	closeOnce sync.Once
	closed    chan struct{}
}

// Dial opens the control channel.
func Dial(addr string) (*Client, error) {
	conn, err := dialHandshake(addr, protocol.HelloControl)
	if err != nil {
		return nil, err
	}
	return &Client{
		addr:     addr,
		ctrlConn: conn,
		ctrl:     rpc.NewClient(conn),
		events:   make(chan protocol.Event, 256),
		closed:   make(chan struct{}),
	}, nil
}

func dialHandshake(addr, hello string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := protocol.WriteLine(conn, hello); err != nil {
		conn.Close()
		return nil, err
	}
	reply, err := protocol.ReadLine(conn, 256)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	conn.SetDeadline(time.Time{})
	if reply != protocol.ReplyOK {
		conn.Close()
		return nil, errors.New(strings.TrimPrefix(reply, protocol.ReplyErr))
	}
	return conn, nil
}

// Join registers the username and opens the push channel. On ErrNameTaken
// (or an invalid name) the client stays connected and Join may be retried.
// Once Join succeeds, events start arriving on Events().
func (c *Client) Join(username string) (*protocol.JoinReply, error) {
	var reply protocol.JoinReply
	err := c.call(protocol.MethodJoin, protocol.JoinArgs{Username: strings.TrimSpace(username), ClientVersion: protocol.Version}, &reply)
	if err != nil {
		return nil, err
	}

	pushConn, err := dialHandshake(c.addr, protocol.HelloPush+reply.Token)
	if err != nil {
		return nil, fmt.Errorf("opening push channel: %w", err)
	}
	c.mu.Lock()
	c.pushConn = pushConn
	c.name = reply.Username
	c.mu.Unlock()

	// Serve the callback service on the push connection: from here on the
	// server is the caller and we are the callee.
	rs := rpc.NewServer()
	if err := rs.RegisterName(protocol.ClientService, &callbacks{c: c}); err != nil {
		pushConn.Close()
		return nil, err
	}
	c.touch()
	go func() {
		rs.ServeConn(pushConn) // returns once all in-flight callbacks finished
		close(c.events)
		if !c.leaving.Load() {
			c.Close() // lost connection or server shutdown
		}
	}()
	if reply.Heartbeat > 0 {
		go c.watchdog(reply.Heartbeat * 3)
	}
	return &reply, nil
}

// Events delivers everything the server pushes. It is closed when the
// connection ends (Leave, server shutdown, or a lost connection).
func (c *Client) Events() <-chan protocol.Event { return c.events }

// Name is the username as registered by the server.
func (c *Client) Name() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.name
}

// Send broadcasts a message to every other user.
func (c *Client) Send(text string) (protocol.SendReply, error) {
	var r protocol.SendReply
	err := c.call(protocol.MethodSend, protocol.SendArgs{Text: text}, &r)
	return r, err
}

// Action broadcasts an emote ("/me waves").
func (c *Client) Action(text string) (protocol.SendReply, error) {
	var r protocol.SendReply
	err := c.call(protocol.MethodSend, protocol.SendArgs{Text: text, Action: true}, &r)
	return r, err
}

// Whisper sends a private message.
func (c *Client) Whisper(to, text string) (protocol.SendReply, error) {
	var r protocol.SendReply
	err := c.call(protocol.MethodWhisper, protocol.WhisperArgs{To: to, Text: text}, &r)
	return r, err
}

// Users lists who is online.
func (c *Client) Users() ([]protocol.UserInfo, error) {
	var r protocol.UsersReply
	err := c.call(protocol.MethodUsers, protocol.Empty{}, &r)
	return r.Users, err
}

// Leave tells the server we are leaving (so others see "left the chat"
// instead of "lost connection"), then closes both channels.
func (c *Client) Leave() error {
	c.leaving.Store(true)
	err := c.callTimeout(protocol.MethodLeave, protocol.Empty{}, &protocol.Empty{}, 2*time.Second)
	c.Close()
	return err
}

// Close drops both connections without saying goodbye.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.ctrl.Close()
		c.mu.Lock()
		if c.pushConn != nil {
			c.pushConn.Close()
		}
		c.mu.Unlock()
	})
	return nil
}

func (c *Client) call(method string, args, reply any) error {
	return c.callTimeout(method, args, reply, callTimeout)
}

func (c *Client) callTimeout(method string, args, reply any, d time.Duration) error {
	call := c.ctrl.Go(method, args, reply, make(chan *rpc.Call, 1))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-call.Done:
		return translate(call.Error)
	case <-t.C:
		return errors.New("the server did not answer in time")
	case <-c.closed:
		return ErrClosed
	}
}

// translate turns net/rpc's string errors back into sentinel values.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, rpc.ErrShutdown) {
		return ErrClosed
	}
	var se rpc.ServerError
	if errors.As(err, &se) && string(se) == protocol.ErrNameTaken {
		return ErrNameTaken
	}
	if errors.As(err, &se) {
		return errors.New(string(se))
	}
	return err
}

func (c *Client) touch() { c.lastSeen.Store(time.Now().UnixNano()) }

// watchdog closes the connection if the server stops sending heartbeats —
// this catches a server that vanished without closing the TCP connection.
func (c *Client) watchdog(limit time.Duration) {
	t := time.NewTicker(limit / 3)
	defer t.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
			if time.Since(time.Unix(0, c.lastSeen.Load())) > limit {
				c.Close()
				return
			}
		}
	}
}

// callbacks is the RPC service the server calls on the push channel.
// The server sends callbacks one at a time per client, so events arrive
// in order even though net/rpc runs each call on its own goroutine.
type callbacks struct{ c *Client }

// Deliver receives one pushed event.
func (cb *callbacks) Deliver(ev protocol.Event, ack *protocol.Ack) error {
	cb.c.touch()
	select {
	case cb.c.events <- ev:
	case <-cb.c.closed:
	}
	ack.Received = true
	return nil
}

// Ping is the server's heartbeat.
func (cb *callbacks) Ping(_ protocol.Empty, ack *protocol.Ack) error {
	cb.c.touch()
	ack.Received = true
	return nil
}
