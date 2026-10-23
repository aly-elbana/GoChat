package protocol

import (
	"bytes"
	"strings"
	"testing"
)

func TestValidateUsername(t *testing.T) {
	good := []string{"al", "alice", "Bob_42", "jean-luc", "a.b", "محمد", "李雷"}
	bad := []string{"", "a", "has space", "_lead", "toolongusername_x", "server", "YOU", "x\x1b"}
	for _, n := range good {
		if err := ValidateUsername(n); err != nil {
			t.Errorf("ValidateUsername(%q) = %v, want ok", n, err)
		}
	}
	for _, n := range bad {
		if err := ValidateUsername(n); err == nil {
			t.Errorf("ValidateUsername(%q) = nil, want error", n)
		}
	}
}

func TestSanitizeMessage(t *testing.T) {
	cases := map[string]string{
		"  hello  ":             "hello",
		"line1\nline2":          "line1 line2",
		"red \x1b[31mtext":      "red [31mtext",
		"bell\a and \u202eevil": "bell and evil",
	}
	for in, want := range cases {
		got, err := SanitizeMessage(in)
		if err != nil || got != want {
			t.Errorf("SanitizeMessage(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := SanitizeMessage(" \t\n "); err == nil {
		t.Error("blank message accepted")
	}
	if _, err := SanitizeMessage(strings.Repeat("x", MaxMessageLen+1)); err == nil {
		t.Error("oversized message accepted")
	}
}

func TestReadLineStopsAtNewline(t *testing.T) {
	r := bytes.NewBufferString("GOCHAT/1 CONTROL\nRPC-BYTES")
	line, err := ReadLine(r, 64)
	if err != nil || line != HelloControl {
		t.Fatalf("got %q, %v", line, err)
	}
	if r.String() != "RPC-BYTES" {
		t.Fatalf("over-read: remaining %q", r.String())
	}
}
