package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubHostAlias replaces the ssh-config lookup for the duration of a test, so
// that naming tests do not depend on whatever aliases this machine happens to
// have.
func stubHostAlias(t *testing.T, aliases map[string]string) {
	t.Helper()
	prev := hostAlias
	hostAlias = func(h string) string { return aliases[strings.ToLower(h)] }
	t.Cleanup(func() { hostAlias = prev })
}

// oldTime is far in the past: a rewrite that moves mtime backwards (a store
// path swapped in by home-manager) must still be picked up.
var oldTime = time.Unix(1, 0)

func TestParseSSHAliases(t *testing.T) {
	cfg := `
# a comment
Host zmb
  HostName 192.168.6.220
  User srv

Host rzn other-name
  hostname 192.168.6.70

Host=slru01
  HostName=195.19.178.75

Host restic
  HostName chat.havent.info
Host zombie
  HostName chat.havent.info

Host *.internal *
  HostName 10.0.0.1

Match host something
  HostName 10.0.0.2

Host templated
  HostName %h.example.com
`
	got := parseSSHAliases(strings.NewReader(cfg))
	want := map[string]string{
		"192.168.6.220":    "zmb",
		"192.168.6.70":     "rzn",
		"195.19.178.75":    "slru01",
		"chat.havent.info": "restic", // the first alias in the file wins
	}
	for host, alias := range want {
		if got[host] != alias {
			t.Errorf("%s: got %q, want %q", host, got[host], alias)
		}
	}
	// A pattern is not a name, a Match block has no alias, and a HostName with
	// a token expands per connection — none of them may end up in the map.
	for _, host := range []string{"10.0.0.1", "10.0.0.2", "%h.example.com"} {
		if alias, ok := got[host]; ok {
			t.Errorf("%s should not resolve, got %q", host, alias)
		}
	}
	if len(got) != len(want) {
		t.Errorf("unexpected entries: %v", got)
	}
}

func TestSSHAliasesReloadOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	a := &sshAliases{path: path}

	// A missing file is not an error, it just has no aliases.
	if got := a.Lookup("192.168.6.70"); got != "" {
		t.Errorf("missing config: got %q", got)
	}

	write("Host rzn\n  HostName 192.168.6.70\n")
	if got := a.Lookup("192.168.6.70"); got != "rzn" {
		t.Errorf("got %q, want rzn", got)
	}
	// Case does not matter to ssh, so it must not matter here either.
	if got := a.Lookup("192.168.6.70"); got != "rzn" {
		t.Errorf("second lookup (cached): got %q, want rzn", got)
	}
	if got := a.Lookup("Chat.Havent.Info"); got != "" {
		t.Errorf("unknown host: got %q", got)
	}

	// home-manager rewrites the file on every switch; the daemon has to notice.
	write("Host rzn\n  HostName 192.168.6.70\nHost zmb\n  HostName 192.168.6.220\n")
	os.Chtimes(path, oldTime, oldTime) // a switch may even move mtime backwards
	if got := a.Lookup("192.168.6.220"); got != "zmb" {
		t.Errorf("after a rewrite: got %q, want zmb", got)
	}
}

func TestSSHNameResolvesAddressesToAliases(t *testing.T) {
	stubHostAlias(t, map[string]string{
		"192.168.6.70":     "rzn",
		"chat.havent.info": "zombie",
	})
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"192.168.6.70"}, "@rzn"},
		{[]string{"uzz@192.168.6.70", "btop"}, "@rzn:btop"},
		{[]string{"ssh://srv@chat.havent.info:2202"}, "@zombie"},
		{[]string{"192.168.6.99"}, "@192.168.6.99"}, // no alias, keep the address
		{[]string{"rzn"}, "@rzn"},                   // already an alias
	}
	for _, c := range cases {
		if got := SSHName(c.args); got != c.want {
			t.Errorf("SSHName(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}
