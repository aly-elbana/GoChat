// Package protocol defines everything the GoChat server and clients agree on:
// the connection handshake, the RPC service and method names, the argument and
// reply types, and the validation rules for usernames and messages.
//
// # Transport overview
//
// Every client opens TWO TCP connections to the same server port:
//
//  1. A control channel ("GOCHAT/1 CONTROL"). The client is the RPC caller and
//     the server is the RPC callee. Join, Send, Whisper, Users and Leave go here.
//
//  2. A push channel ("GOCHAT/1 PUSH <token>"). The roles are reversed: the
//     client serves RPC and the server calls back into it (Client.Deliver,
//     Client.Ping). This is how the server pushes messages and notifications
//     the instant they happen, with no polling.
//
// Because the client dials both connections, the push channel works through
// NAT and firewalls: the server never has to open a connection to a client.
package protocol

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Version is the application version shown by both programs.
const Version = "1.0.0"

// Handshake lines. Each connection starts with exactly one of these, followed
// by "\n". The server answers "OK\n" or "ERR <reason>\n" before RPC traffic.
const (
	HelloControl = "GOCHAT/1 CONTROL"
	HelloPush    = "GOCHAT/1 PUSH " // followed by the session token
	ReplyOK      = "OK"
	ReplyErr     = "ERR "
)

// RPC method names.
const (
	ServerService = "Chat"
	MethodJoin    = ServerService + ".Join"
	MethodSend    = ServerService + ".Send"
	MethodWhisper = ServerService + ".Whisper"
	MethodUsers   = ServerService + ".Users"
	MethodLeave   = ServerService + ".Leave"

	ClientService = "Client"
	MethodDeliver = ClientService + ".Deliver"
	MethodPing    = ClientService + ".Ping"
)

// Limits.
const (
	MinUsernameLen = 2
	MaxUsernameLen = 16
	MaxMessageLen  = 1000 // in runes
)

// Error messages returned by the server. net/rpc transports errors as plain
// strings, so clients compare against these values.
const (
	ErrNameTaken     = "username is already taken"
	ErrNotJoined     = "you have not joined the chat"
	ErrAlreadyJoined = "already joined"
	ErrNoSuchUser    = "no such user is online"
	ErrWhisperSelf   = "you can't whisper to yourself"
	ErrEmptyMessage  = "message is empty"
	ErrTooLong       = "message is too long"
	ErrShuttingDown  = "server is shutting down"
	ErrBadToken      = "unknown or expired session token"
)

// EventKind tells the client how to render an Event.
type EventKind int

const (
	EventMessage  EventKind = iota // public message
	EventAction                    // "/me waves"
	EventPrivate                   // direct message to one user
	EventJoin                      // a user joined
	EventLeave                     // a user left or was disconnected
	EventSystem                    // informational text from the server
	EventReady                     // history replay finished; live from here on
	EventShutdown                  // the server is shutting down
)

func (k EventKind) String() string {
	switch k {
	case EventMessage:
		return "message"
	case EventAction:
		return "action"
	case EventPrivate:
		return "private"
	case EventJoin:
		return "join"
	case EventLeave:
		return "leave"
	case EventSystem:
		return "system"
	case EventReady:
		return "ready"
	case EventShutdown:
		return "shutdown"
	}
	return fmt.Sprintf("EventKind(%d)", int(k))
}

// Reasons attached to EventLeave.
const (
	ReasonQuit     = "left the chat"
	ReasonLost     = "lost connection"
	ReasonTimeout  = "timed out"
	ReasonSlow     = "was too slow to keep up"
	ReasonShutdown = "server shutdown"
)

// Event is what the server pushes to clients through Client.Deliver.
type Event struct {
	Seq    uint64    // global, monotonically increasing ordering number
	Kind   EventKind //
	From   string    // author (message/action/private) or subject (join/leave)
	To     string    // recipient of a private message
	Text   string    // message body or system text
	Reason string    // why a user left (EventLeave)
	Online int       // users online after a join/leave
	Time   time.Time //
	Replay bool      // true when re-sent as part of the join history
}

// UserInfo describes one connected user.
type UserInfo struct {
	Name     string
	JoinedAt time.Time
}

// Empty is used where an RPC needs no arguments or reply. (encoding/gob
// refuses structs without exported fields, hence the placeholder.)
type Empty struct{ Nothing bool }

// Ack is the reply to a callback.
type Ack struct{ Received bool }

// JoinArgs is the argument to Chat.Join.
type JoinArgs struct {
	Username      string
	ClientVersion string
}

// JoinReply is returned by a successful Chat.Join.
type JoinReply struct {
	Username      string     // the name as registered
	Token         string     // opens the push channel; single use, short-lived
	ServerName    string     //
	ServerVersion string     //
	MOTD          string     // message of the day
	Users         []UserInfo // users already online (not including you)
	Heartbeat     time.Duration
}

// SendArgs is the argument to Chat.Send.
type SendArgs struct {
	Text   string
	Action bool // render as "* name text"
}

// WhisperArgs is the argument to Chat.Whisper.
type WhisperArgs struct {
	To   string
	Text string
}

// SendReply is returned by Chat.Send and Chat.Whisper.
type SendReply struct {
	Seq        uint64
	Time       time.Time
	Recipients int    // how many users the message was queued for
	Text       string // the message as stored (sanitised)
	To         string // canonical recipient name for whispers
}

// UsersReply is returned by Chat.Users.
type UsersReply struct {
	Users []UserInfo
}

var reservedNames = map[string]bool{
	"server": true, "system": true, "admin": true, "you": true,
	"all": true, "everyone": true, "gochat": true,
}

// ValidateUsername reports whether name may be used, with a friendly reason
// when it can't.
func ValidateUsername(name string) error {
	n := utf8.RuneCountInString(name)
	switch {
	case n == 0:
		return errors.New("invalid username: it can't be empty")
	case n < MinUsernameLen:
		return fmt.Errorf("invalid username: use at least %d characters", MinUsernameLen)
	case n > MaxUsernameLen:
		return fmt.Errorf("invalid username: use at most %d characters", MaxUsernameLen)
	}
	for i, r := range name {
		ok := unicode.IsLetter(r) || unicode.IsDigit(r) || (i > 0 && (r == '_' || r == '-' || r == '.'))
		if !ok {
			return errors.New("invalid username: use letters, digits, and _ - . (starting with a letter or digit)")
		}
	}
	if reservedNames[strings.ToLower(name)] {
		return fmt.Errorf("invalid username: %q is reserved", name)
	}
	return nil
}

// NameKey is the case-insensitive identity of a username, so "Alice" and
// "alice" can't both be online.
func NameKey(name string) string { return strings.ToLower(name) }

// SanitizeMessage strips terminal control characters (so nobody can inject
// ANSI escape codes into other people's terminals), collapses tabs/newlines
// to spaces and enforces the length limits.
func SanitizeMessage(text string) (string, error) {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteRune(' ')
		case r == utf8.RuneError:
			// drop invalid UTF-8
		case unicode.IsControl(r):
			// drop C0/C1 control characters, including ESC
		case r == '\u200b' || r == '\u2028' || r == '\u2029' || r == '\u202e' || r == '\u202d':
			// drop zero-width and bidi-override tricks
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", errors.New(ErrEmptyMessage)
	}
	if utf8.RuneCountInString(out) > MaxMessageLen {
		return "", fmt.Errorf("%s (max %d characters)", ErrTooLong, MaxMessageLen)
	}
	return out, nil
}

// ReadLine reads a single handshake line one byte at a time. Reading byte by
// byte matters: it guarantees we never consume any RPC bytes that follow the
// newline, so the connection can be handed straight to net/rpc afterwards.
func ReadLine(r io.Reader, max int) (string, error) {
	var buf []byte
	one := make([]byte, 1)
	for len(buf) < max {
		n, err := r.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				return strings.TrimRight(string(buf), "\r"), nil
			}
			buf = append(buf, one[0])
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("handshake line too long")
}

// WriteLine writes a handshake line.
func WriteLine(w io.Writer, line string) error {
	bw := bufio.NewWriter(w)
	if _, err := bw.WriteString(line + "\n"); err != nil {
		return err
	}
	return bw.Flush()
}
