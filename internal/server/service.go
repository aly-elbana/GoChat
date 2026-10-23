package server

import (
	"errors"
	"net"
	"sync"

	"gochat/internal/protocol"
)

// ChatService is the RPC service exposed on a client's control connection.
// A fresh instance is created per connection, so it always knows who is
// calling. net/rpc may run several calls from one connection concurrently,
// hence the mutex.
type ChatService struct {
	hub    *hub
	cfg    Config
	conn   net.Conn
	remote string

	mu   sync.Mutex
	sess *session
}

func (c *ChatService) session() (*session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == nil {
		return nil, errors.New(protocol.ErrNotJoined)
	}
	return c.sess, nil
}

// Join claims a unique username and returns the token that opens the push
// channel. A failed Join leaves the connection usable, so a client can retry
// with another name.
func (c *ChatService) Join(args protocol.JoinArgs, reply *protocol.JoinReply) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess != nil && !c.sessRemoved() {
		return errors.New(protocol.ErrAlreadyJoined)
	}
	sess, users, err := c.hub.reserve(args.Username, c.remote, c.conn)
	if err != nil {
		return err
	}
	c.sess = sess
	*reply = protocol.JoinReply{
		Username:      sess.name,
		Token:         sess.token,
		ServerName:    c.cfg.Name,
		ServerVersion: protocol.Version,
		MOTD:          c.cfg.MOTD,
		Users:         users,
		Heartbeat:     c.cfg.PingInterval,
	}
	return nil
}

func (c *ChatService) sessRemoved() bool {
	c.hub.mu.Lock()
	defer c.hub.mu.Unlock()
	return c.sess.removed
}

// Send broadcasts a message to everyone except the sender.
func (c *ChatService) Send(args protocol.SendArgs, reply *protocol.SendReply) error {
	s, err := c.session()
	if err != nil {
		return err
	}
	r, err := c.hub.send(s, args.Text, args.Action)
	if err != nil {
		return err
	}
	*reply = r
	return nil
}

// Whisper sends a private message to one user.
func (c *ChatService) Whisper(args protocol.WhisperArgs, reply *protocol.SendReply) error {
	s, err := c.session()
	if err != nil {
		return err
	}
	r, err := c.hub.whisper(s, args.To, args.Text)
	if err != nil {
		return err
	}
	*reply = r
	return nil
}

// Users lists the users currently online.
func (c *ChatService) Users(_ protocol.Empty, reply *protocol.UsersReply) error {
	if _, err := c.session(); err != nil {
		return err
	}
	reply.Users = c.hub.users()
	return nil
}

// Leave removes the caller from the chat and notifies everyone else.
func (c *ChatService) Leave(_ protocol.Empty, _ *protocol.Empty) error {
	s, err := c.session()
	if err != nil {
		return err
	}
	c.hub.remove(s, protocol.ReasonQuit)
	return nil
}

// disconnected runs when the control connection closes. If the user never
// called Leave, this was an unexpected disconnection (crash, kill -9, network
// drop), and the others are told so.
func (c *ChatService) disconnected() {
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s != nil {
		c.hub.remove(s, protocol.ReasonLost)
	}
}
