package main

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// A tab named "@192.168.6.70" says nothing; "@rzn" is the name the connection
// was actually typed under. ssh resolves an alias to a HostName, so the map
// here is walked the other way round: HostName -> the first alias declaring it.
//
// The file is re-read when it changes on disk — home-manager rewrites it on
// every switch, and a daemon that cached it at startup would keep naming tabs
// after an address that has a name now.
type sshAliases struct {
	mu      sync.Mutex
	path    string
	loaded  bool
	modTime int64
	size    int64
	byHost  map[string]string
}

// defaultSSHAliases reads the user's own ~/.ssh/config. The system-wide
// /etc/ssh/ssh_config is deliberately left out: it holds defaults, not the
// per-host aliases a person types.
var defaultSSHAliases = &sshAliases{}

func (a *sshAliases) configPath() string {
	if a.path != "" {
		return a.path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".ssh", "config")
}

// Lookup returns the alias declared for a host name or address, or an empty
// string when the config names none.
func (a *sshAliases) Lookup(host string) string {
	if host == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reloadIfChanged()
	return a.byHost[strings.ToLower(host)]
}

// reloadIfChanged re-parses the config when its size or mtime moved. A missing
// or unreadable file is not an error — it only means there are no aliases.
func (a *sshAliases) reloadIfChanged() {
	path := a.configPath()
	if path == "" {
		a.byHost, a.loaded = nil, true
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		a.byHost, a.loaded = nil, true
		a.modTime, a.size = 0, 0
		return
	}
	if a.loaded && st.ModTime().UnixNano() == a.modTime && st.Size() == a.size {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		a.byHost, a.loaded = nil, true
		return
	}
	defer f.Close()
	a.byHost = parseSSHAliases(f)
	a.loaded = true
	a.modTime, a.size = st.ModTime().UnixNano(), st.Size()
}

// parseSSHAliases maps HostName values to the alias that declares them. The
// first alias in the file wins, so two aliases pointing at one address (a
// service account next to a login one) resolve deterministically.
func parseSSHAliases(r io.Reader) map[string]string {
	out := map[string]string{}
	var aliases []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// "Host zmb", "Host=zmb" and "HostName 1.2.3.4" are all valid spellings.
		key, value, ok := strings.Cut(strings.ReplaceAll(line, "=", " "), " ")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(key) {
		case "host":
			aliases = nil
			for _, pattern := range strings.Fields(value) {
				// A pattern is not a name one can put in a tab.
				if strings.ContainsAny(pattern, "*?!") {
					continue
				}
				aliases = append(aliases, pattern)
			}
		case "match":
			// A Match block has no alias of its own, and its HostName applies
			// to whatever matched — nothing to name a tab after.
			aliases = nil
		case "hostname":
			if len(aliases) == 0 || value == "" || strings.Contains(value, "%") {
				continue
			}
			host := strings.ToLower(strings.Trim(value, `"`))
			if _, seen := out[host]; !seen {
				out[host] = aliases[0]
			}
		}
	}
	return out
}

// hostAlias is the seam the tests replace; in production it reads the user's
// ssh config.
var hostAlias = defaultSSHAliases.Lookup
