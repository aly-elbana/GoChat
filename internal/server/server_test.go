package server_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"sync"
	"testing"
	"time"

	"gochat/internal/client"
	"gochat/internal/protocol"
	"gochat/internal/server"
)

func startServer(t *testing.T, cfg server.Config) (*server.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg)
	go srv.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return srv, ln.Addr().String()
}

func join(t *testing.T, addr, name string) *client.Client {
	t.Helper()
	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := c.Join(name); err != nil {
		t.Fatalf("join %s: %v", name, err)
	}
	t.Cleanup(func() { c.Close() })
	waitFor(t, c, func(ev protocol.Event) bool { return ev.Kind == protocol.EventReady })
	return c
}

// waitFor skips events until one matches.
func waitFor(t *testing.T, c *client.Client, match func(protocol.Event) bool) protocol.Event {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				t.Fatalf("%s: event stream closed while waiting", c.Name())
			}
			if match(ev) {
				return ev
			}
		case <-timeout:
			t.Fatalf("%s: timed out waiting for event", c.Name())
		}
	}
}

// expectNone asserts that no matching event arrives within d.
func expectNone(t *testing.T, c *client.Client, d time.Duration, match func(protocol.Event) bool) {
	t.Helper()
	timeout := time.After(d)
	for {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				return
			}
			if match(ev) {
				t.Fatalf("%s: unexpected event %+v", c.Name(), ev)
			}
		case <-timeout:
			return
		}
	}
}

func isMsg(from, text string) func(protocol.Event) bool {
	return func(ev protocol.Event) bool {
		return ev.Kind == protocol.EventMessage && ev.From == from && ev.Text == text
	}
}

func TestBroadcastExcludesSender(t *testing.T) {
	_, addr := startServer(t, server.Config{})
	alice := join(t, addr, "alice")
	bob := join(t, addr, "bob")
	carol := join(t, addr, "carol")

	r, err := alice.Send("hello everyone")
	if err != nil {
		t.Fatal(err)
	}
	if r.Recipients != 2 {
		t.Errorf("recipients = %d, want 2", r.Recipients)
	}
	waitFor(t, bob, isMsg("alice", "hello everyone"))
	waitFor(t, carol, isMsg("alice", "hello everyone"))
	expectNone(t, alice, 300*time.Millisecond, func(ev protocol.Event) bool { return ev.Kind == protocol.EventMessage })
}

func TestJoinAndLeaveNotifications(t *testing.T) {
	_, addr := startServer(t, server.Config{})
	alice := join(t, addr, "alice")
	bob := join(t, addr, "bob")

	ev := waitFor(t, alice, func(ev protocol.Event) bool { return ev.Kind == protocol.EventJoin })
	if ev.From != "bob" || ev.Online != 2 {
		t.Fatalf("join event = %+v", ev)
	}
	if err := bob.Leave(); err != nil {
		t.Fatal(err)
	}
	ev = waitFor(t, alice, func(ev protocol.Event) bool { return ev.Kind == protocol.EventLeave })
	if ev.From != "bob" || ev.Reason != protocol.ReasonQuit || ev.Online != 1 {
		t.Fatalf("leave event = %+v", ev)
	}
}

func TestDuplicateUsernameIsRejectedAndRetryWorks(t *testing.T) {
	_, addr := startServer(t, server.Config{})
	join(t, addr, "alice")

	c, err := client.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Join("ALICE"); !errors.Is(err, client.ErrNameTaken) {
		t.Fatalf("join ALICE: err = %v, want ErrNameTaken", err)
	}
	// Same connection, another name.
	if _, err := c.Join("alice2"); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestInvalidUsernames(t *testing.T) {
	_, addr := startServer(t, server.Config{})
	c, err := client.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, name := range []string{"", "a", "has space", "averyveryverylongname", "server", "-dash", "bad\x1b[31m"} {
		if _, err := c.Join(name); err == nil {
			t.Errorf("Join(%q) succeeded, want error", name)
		}
	}
}

func TestUnexpectedDisconnectNotifiesOthers(t *testing.T) {
	_, addr := startServer(t, server.Config{})
	alice := join(t, addr, "alice")
	bob := join(t, addr, "bob")

	bob.Close() // like a crash / kill -9: no Leave call

	ev := waitFor(t, alice, func(ev protocol.Event) bool { return ev.Kind == protocol.EventLeave })
	if ev.From != "bob" || ev.Reason != protocol.ReasonLost {
		t.Fatalf("leave event = %+v", ev)
	}
	// The name is free again.
	join(t, addr, "bob")
}

func TestUsersList(t *testing.T) {
	_, addr := startServer(t, server.Config{})
	alice := join(t, addr, "alice")
	join(t, addr, "bob")
	join(t, addr, "carol")
	users, err := alice.Users()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, u := range users {
		names = append(names, u.Name)
	}
	if fmt.Sprint(names) != "[alice bob carol]" {
		t.Fatalf("users = %v", names)
	}
}

func TestWhisperOnlyReachesTarget(t *testing.T) {
	_, addr := startServer(t, server.Config{})
	alice := join(t, addr, "alice")
	bob := join(t, addr, "bob")
	carol := join(t, addr, "carol")

	if _, err := alice.Whisper("BOB", "psst"); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, bob, func(ev protocol.Event) bool { return ev.Kind == protocol.EventPrivate })
	if ev.From != "alice" || ev.To != "bob" || ev.Text != "psst" {
		t.Fatalf("whisper = %+v", ev)
	}
	expectNone(t, carol, 300*time.Millisecond, func(ev protocol.Event) bool { return ev.Kind == protocol.EventPrivate })
	if _, err := alice.Whisper("nobody", "hi"); err == nil {
		t.Error("whisper to unknown user succeeded")
	}
	if _, err := alice.Whisper("alice", "hi"); err == nil {
		t.Error("whisper to self succeeded")
	}
}

func TestMessagesAreSanitized(t *testing.T) {
	_, addr := startServer(t, server.Config{})
	alice := join(t, addr, "alice")
	bob := join(t, addr, "bob")
	if _, err := alice.Send("hi \x1b[2Jthere"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, bob, isMsg("alice", "hi [2Jthere"))
	if _, err := alice.Send("   "); err == nil {
		t.Error("empty message accepted")
	}
}

func TestHistoryIsReplayedToNewcomers(t *testing.T) {
	_, addr := startServer(t, server.Config{HistorySize: 2})
	alice := join(t, addr, "alice")
	for _, m := range []string{"one", "two", "three"} {
		if _, err := alice.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Join("bob"); err != nil {
		t.Fatal(err)
	}
	var got []string
	for ev := range c.Events() {
		if ev.Kind == protocol.EventReady {
			break
		}
		if !ev.Replay {
			t.Fatalf("expected replay event, got %+v", ev)
		}
		got = append(got, ev.Text)
	}
	if fmt.Sprint(got) != "[two three]" {
		t.Fatalf("history = %v", got)
	}
}

func TestManyClientsConcurrently(t *testing.T) {
	const clients, perClient = 8, 50
	_, addr := startServer(t, server.Config{})
	cs := make([]*client.Client, clients)
	for i := range cs {
		cs[i] = join(t, addr, fmt.Sprintf("user%d", i))
	}

	// Each receiver tracks, per sender, the next message number it expects:
	// this verifies both completeness and per-sender ordering.
	var recv sync.WaitGroup
	errs := make(chan error, clients)
	for i, c := range cs {
		recv.Add(1)
		go func(i int, c *client.Client) {
			defer recv.Done()
			next := map[string]int{}
			want := (clients - 1) * perClient
			deadline := time.After(10 * time.Second)
			for got := 0; got < want; {
				select {
				case ev := <-c.Events():
					if ev.Kind != protocol.EventMessage {
						continue
					}
					if ev.From == c.Name() {
						errs <- fmt.Errorf("%s received its own message", c.Name())
						return
					}
					exp := fmt.Sprintf("%s #%d", ev.From, next[ev.From])
					if ev.Text != exp {
						errs <- fmt.Errorf("%s: got %q, want %q", c.Name(), ev.Text, exp)
						return
					}
					next[ev.From]++
					got++
				case <-deadline:
					errs <- fmt.Errorf("%s: timed out", c.Name())
					return
				}
			}
		}(i, c)
	}

	var send sync.WaitGroup
	for _, c := range cs {
		send.Add(1)
		go func(c *client.Client) {
			defer send.Done()
			for j := 0; j < perClient; j++ {
				if _, err := c.Send(fmt.Sprintf("%s #%d", c.Name(), j)); err != nil {
					errs <- err
					return
				}
			}
		}(c)
	}
	send.Wait()
	recv.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestShutdownNotifiesClients(t *testing.T) {
	srv, addr := startServer(t, server.Config{})
	alice := join(t, addr, "alice")
	bob := join(t, addr, "bob")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*client.Client{alice, bob} {
		waitFor(t, c, func(ev protocol.Event) bool { return ev.Kind == protocol.EventShutdown })
		select {
		case _, ok := <-c.Events():
			for ok {
				_, ok = <-c.Events()
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: event stream not closed after shutdown", c.Name())
		}
	}
	if _, err := client.Dial(addr); err == nil {
		t.Error("dial succeeded after shutdown")
	}
}

func TestUnresponsiveClientIsDropped(t *testing.T) {
	_, addr := startServer(t, server.Config{PingInterval: 100 * time.Millisecond, CallTimeout: 200 * time.Millisecond})
	alice := join(t, addr, "alice")

	// A raw "client" that joins and opens a push channel but never answers
	// callbacks — like a frozen process.
	closeFrozen, err := rawJoinWithoutServing(addr, "frozen")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFrozen()

	waitFor(t, alice, func(ev protocol.Event) bool { return ev.Kind == protocol.EventJoin && ev.From == "frozen" })
	ev := waitFor(t, alice, func(ev protocol.Event) bool { return ev.Kind == protocol.EventLeave && ev.From == "frozen" })
	if ev.Reason != protocol.ReasonTimeout {
		t.Fatalf("reason = %q, want %q", ev.Reason, protocol.ReasonTimeout)
	}
}

func rawJoinWithoutServing(addr, name string) (func(), error) {
	ctrl, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	protocol.WriteLine(ctrl, protocol.HelloControl)
	if l, err := protocol.ReadLine(ctrl, 64); err != nil || l != protocol.ReplyOK {
		return nil, fmt.Errorf("control handshake: %q %v", l, err)
	}
	rc := rpc.NewClient(ctrl)
	var reply protocol.JoinReply
	if err := rc.Call(protocol.MethodJoin, protocol.JoinArgs{Username: name}, &reply); err != nil {
		return nil, err
	}
	push, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	protocol.WriteLine(push, protocol.HelloPush+reply.Token)
	if l, err := protocol.ReadLine(push, 64); err != nil || l != protocol.ReplyOK {
		return nil, fmt.Errorf("push handshake: %q %v", l, err)
	}
	// Never read from push again: callbacks go unanswered.
	return func() { rc.Close(); push.Close() }, nil
}
