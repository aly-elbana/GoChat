package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/rpc"
	"sort"
	"sync"
	"time"

	"gochat/internal/protocol"
)

// session is one connected user.
//
// Every field below `ctrl` is guarded by hub.mu. The outbox channel is only
// ever sent to and closed while holding hub.mu, which is what makes closing
// it safe; the session's writer goroutine is its only receiver.
type session struct {
	name   string // as the user typed it
	key    string // case-insensitive identity
	remote string
	joined time.Time
	ctrl   net.Conn // control connection (client -> server RPC)

	token       string
	attachTimer *time.Timer
	push        *rpc.Client // server -> client callback RPC
	pushConn    net.Conn
	outbox      chan protocol.Event
	active      bool // push channel attached; visible to others
	removed     bool
	sent        int
}

// hub owns the registry of users and fans messages out to them.
//
// Concurrency design:
//   - one mutex (mu) protects the registry, the history ring and every outbox
//     send/close; holding it while broadcasting gives every client the same
//     global message order;
//   - outbox sends never block (select/default), so a slow or dead client
//     can't stall a broadcast — it gets disconnected instead;
//   - each session has a dedicated writer goroutine that drains its outbox
//     and performs the blocking RPC callbacks outside the lock.
type hub struct {
	cfg Config
	log *Logger

	mu       sync.Mutex
	byKey    map[string]*session // includes sessions still waiting to attach
	byToken  map[string]*session // only sessions waiting to attach
	history  []protocol.Event
	seq      uint64
	shutting bool

	writers sync.WaitGroup

	// stats
	totalJoins    int
	totalMessages int
	peakOnline    int
}

func newHub(cfg Config, log *Logger) *hub {
	return &hub{
		cfg:     cfg,
		log:     log,
		byKey:   make(map[string]*session),
		byToken: make(map[string]*session),
	}
}

var (
	errTimeout = errors.New("callback timed out")
)

// reserve validates the name and claims it. The user stays invisible until
// their push channel is attached.
func (h *hub) reserve(name, remote string, ctrl net.Conn) (*session, []protocol.UserInfo, error) {
	if err := protocol.ValidateUsername(name); err != nil {
		h.log.Warn("rejected %q from %s: %v", name, remote, err)
		return nil, nil, err
	}
	token, err := newToken()
	if err != nil {
		return nil, nil, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shutting {
		return nil, nil, errors.New(protocol.ErrShuttingDown)
	}
	key := protocol.NameKey(name)
	if _, taken := h.byKey[key]; taken {
		h.log.Warn("rejected %q from %s: username taken", name, remote)
		return nil, nil, errors.New(protocol.ErrNameTaken)
	}
	s := &session{
		name:   name,
		key:    key,
		remote: remote,
		ctrl:   ctrl,
		token:  token,
		outbox: make(chan protocol.Event, h.cfg.OutboxSize),
	}
	h.byKey[key] = s
	h.byToken[token] = s
	s.attachTimer = time.AfterFunc(h.cfg.AttachTimeout, func() { h.expire(s) })
	return s, h.usersLocked(), nil
}

// expire drops a reservation whose push channel never showed up.
func (h *hub) expire(s *session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !s.active && !s.removed {
		h.log.Warn("%s never opened a push channel; releasing the name", s.name)
		h.removeLocked(s, protocol.ReasonTimeout)
	}
}

// attach binds a push connection to a reserved session, replays recent
// history, announces the user and returns the session ready for its writer.
func (h *hub) attach(token string, conn net.Conn) (*session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.byToken[token]
	if !ok || s.removed {
		return nil, errors.New(protocol.ErrBadToken)
	}
	if h.shutting {
		return nil, errors.New(protocol.ErrShuttingDown)
	}
	delete(h.byToken, token)
	s.attachTimer.Stop()
	s.token = ""
	s.push = rpc.NewClient(conn)
	s.pushConn = conn
	s.active = true
	s.joined = time.Now()

	// History first, then the "you're live" marker. Everything is queued
	// under the same lock as broadcasts, so there is no gap and no overlap
	// between what was replayed and what arrives live.
	for _, ev := range h.history {
		ev.Replay = true
		h.enqueueLocked(s, ev)
	}
	online := h.onlineLocked()
	h.enqueueLocked(s, protocol.Event{Kind: protocol.EventReady, Online: online, Time: time.Now()})

	h.totalJoins++
	if online > h.peakOnline {
		h.peakOnline = online
	}
	h.broadcastLocked(s, protocol.Event{Kind: protocol.EventJoin, From: s.name, Online: online})
	h.log.Join("%s from %s %s", s.name, s.remote, h.log.Paint(cDim, plural(online, "user", "users")+" online"))
	h.writers.Add(1)
	return s, nil
}

// send broadcasts a public message (or /me action) from s.
func (h *hub) send(s *session, text string, action bool) (protocol.SendReply, error) {
	text, err := protocol.SanitizeMessage(text)
	if err != nil {
		return protocol.SendReply{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !s.active || s.removed {
		return protocol.SendReply{}, errors.New(protocol.ErrNotJoined)
	}
	kind := protocol.EventMessage
	if action {
		kind = protocol.EventAction
	}
	ev := h.broadcastLocked(s, protocol.Event{Kind: kind, From: s.name, Text: text})
	s.sent++
	h.totalMessages++
	n := h.onlineLocked() - 1
	if action {
		h.log.Msg("%s → %s %s", s.name, plural(n, "recipient", "recipients"), h.log.Paint(cDim, "* "+s.name+" "+preview(text)))
	} else {
		h.log.Msg("%s → %s %s", s.name, plural(n, "recipient", "recipients"), h.log.Paint(cDim, preview(text)))
	}
	return protocol.SendReply{Seq: ev.Seq, Time: ev.Time, Recipients: n, Text: text}, nil
}

// whisper delivers a private message from s to one user.
func (h *hub) whisper(s *session, to, text string) (protocol.SendReply, error) {
	text, err := protocol.SanitizeMessage(text)
	if err != nil {
		return protocol.SendReply{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !s.active || s.removed {
		return protocol.SendReply{}, errors.New(protocol.ErrNotJoined)
	}
	target, ok := h.byKey[protocol.NameKey(to)]
	if !ok || !target.active {
		return protocol.SendReply{}, errors.New(protocol.ErrNoSuchUser)
	}
	if target == s {
		return protocol.SendReply{}, errors.New(protocol.ErrWhisperSelf)
	}
	h.seq++
	ev := protocol.Event{Seq: h.seq, Kind: protocol.EventPrivate, From: s.name, To: target.name, Text: text, Time: time.Now()}
	if !h.enqueueLocked(target, ev) {
		h.removeLocked(target, protocol.ReasonSlow)
		return protocol.SendReply{}, errors.New(protocol.ErrNoSuchUser)
	}
	s.sent++
	h.totalMessages++
	h.log.DM("%s → %s", s.name, target.name)
	return protocol.SendReply{Seq: ev.Seq, Time: ev.Time, Recipients: 1, Text: text, To: target.name}, nil
}

// users lists everyone online, oldest first.
func (h *hub) users() []protocol.UserInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.usersLocked()
}

func (h *hub) usersLocked() []protocol.UserInfo {
	out := make([]protocol.UserInfo, 0, len(h.byKey))
	for _, s := range h.byKey {
		if s.active {
			out = append(out, protocol.UserInfo{Name: s.name, JoinedAt: s.joined})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JoinedAt.Before(out[j].JoinedAt) })
	return out
}

func (h *hub) onlineLocked() int {
	n := 0
	for _, s := range h.byKey {
		if s.active {
			n++
		}
	}
	return n
}

// remove takes a user out of the chat (idempotent).
func (h *hub) remove(s *session, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeLocked(s, reason)
}

func (h *hub) removeLocked(s *session, reason string) {
	if s.removed {
		return
	}
	s.removed = true
	delete(h.byKey, s.key)
	if s.token != "" {
		delete(h.byToken, s.token)
	}
	if s.attachTimer != nil {
		s.attachTimer.Stop()
	}
	close(s.outbox) // the writer drains what's left, then exits

	switch reason {
	case protocol.ReasonShutdown:
		// keep the push channel open so the shutdown notice can be delivered
	case protocol.ReasonQuit:
		if s.pushConn != nil {
			s.pushConn.Close()
		}
	default:
		// Involuntary: tear both channels down so the client notices too.
		if s.pushConn != nil {
			s.pushConn.Close()
		}
		if s.ctrl != nil {
			s.ctrl.Close()
		}
	}

	if !s.active || reason == protocol.ReasonShutdown {
		return
	}
	online := h.onlineLocked()
	if !h.shutting {
		h.broadcastLocked(nil, protocol.Event{Kind: protocol.EventLeave, From: s.name, Reason: reason, Online: online})
	}
	if reason == protocol.ReasonQuit {
		h.log.Leave("%s %s", s.name, h.log.Paint(cDim, plural(online, "user", "users")+" online"))
	} else {
		h.log.Drop("%s %s %s", s.name, reason, h.log.Paint(cDim, plural(online, "user", "users")+" online"))
	}
}

// broadcastLocked stamps ev and queues it for every active user except
// `except`. Users whose outbox is full are disconnected afterwards.
func (h *hub) broadcastLocked(except *session, ev protocol.Event) protocol.Event {
	h.seq++
	ev.Seq = h.seq
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if ev.Kind == protocol.EventMessage || ev.Kind == protocol.EventAction {
		h.history = append(h.history, ev)
		if over := len(h.history) - h.cfg.HistorySize; over > 0 {
			h.history = append(h.history[:0:0], h.history[over:]...)
		}
	}
	var slow []*session
	for _, s := range h.byKey {
		if s == except || !s.active || s.removed {
			continue
		}
		if !h.enqueueLocked(s, ev) {
			slow = append(slow, s)
		}
	}
	for _, s := range slow {
		h.removeLocked(s, protocol.ReasonSlow)
	}
	return ev
}

func (h *hub) enqueueLocked(s *session, ev protocol.Event) bool {
	if s.removed {
		return false
	}
	select {
	case s.outbox <- ev:
		return true
	default:
		return false
	}
}

// runWriter is the per-user goroutine that performs the server -> client
// callbacks. It owns the push RPC client: deliveries are made one at a time,
// which preserves ordering, and a heartbeat ping detects dead peers.
func (h *hub) runWriter(s *session) {
	defer h.writers.Done()
	defer s.push.Close()
	ping := time.NewTicker(h.cfg.PingInterval)
	defer ping.Stop()
	for {
		select {
		case ev, ok := <-s.outbox:
			if !ok {
				return
			}
			if err := h.callback(s, protocol.MethodDeliver, ev); err != nil {
				h.remove(s, reasonFor(err))
				return
			}
		case <-ping.C:
			if err := h.callback(s, protocol.MethodPing, protocol.Empty{}); err != nil {
				h.remove(s, reasonFor(err))
				return
			}
		}
	}
}

func (h *hub) callback(s *session, method string, args any) error {
	call := s.push.Go(method, args, &protocol.Ack{}, make(chan *rpc.Call, 1))
	timer := time.NewTimer(h.cfg.CallTimeout)
	defer timer.Stop()
	select {
	case <-call.Done:
		return call.Error
	case <-timer.C:
		return errTimeout
	}
}

func reasonFor(err error) string {
	if errors.Is(err, errTimeout) {
		return protocol.ReasonTimeout
	}
	return protocol.ReasonLost
}

// shutdown tells every user the server is going away and closes their
// outboxes so the writers flush the notice and exit.
func (h *hub) shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shutting {
		return
	}
	h.shutting = true
	h.seq++
	notice := protocol.Event{Seq: h.seq, Kind: protocol.EventShutdown, Text: "the server is shutting down", Time: time.Now()}
	for _, s := range h.byKey {
		if s.active {
			h.enqueueLocked(s, notice)
		}
		h.removeLocked(s, protocol.ReasonShutdown)
	}
}

func (h *hub) stats() (online, joins, messages, peak int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.onlineLocked(), h.totalJoins, h.totalMessages, h.peakOnline
}

func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
