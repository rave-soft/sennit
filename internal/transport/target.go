// Package transport lets a wsrpc client reach a daemon over SSH instead
// of a local unix socket (CLIENT-SERVER.md, PR 3.1). It must not import
// internal/app, internal/agent or internal/db: it is linked into the
// client binary path, and CLIENT-SERVER.md's owner decision ("бинарь")
// only holds if that path stays free of the backend.
package transport

import (
	"fmt"
	"net/url"
	"strings"
)

// Target is a parsed `ssh://[user@]host[:port]/abs/path` remote target:
// a machine to reach over SSH, and the absolute path on it of the
// project whose daemon `sennit attach`/`--remote`/`ps --remote` wants.
type Target struct {
	User string
	Host string
	Port string // "" when the target names no explicit port.
	Path string
}

// ParseTarget parses raw as an `ssh://[user@]host[:port]/abs/path`
// remote target. Any other scheme, or a path that isn't absolute, is
// rejected with an error naming what was wrong -- both are user input
// (a CLI flag/argument), so the message is meant to be read, not just
// logged.
func ParseTarget(raw string) (Target, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Target{}, fmt.Errorf("parsing remote target %q: %w", raw, err)
	}
	if u.Scheme != "ssh" {
		return Target{}, fmt.Errorf("remote target %q must use the ssh:// scheme, got %q", raw, u.Scheme)
	}
	if u.Host == "" {
		return Target{}, fmt.Errorf("remote target %q has no host", raw)
	}
	if u.Path == "" || !strings.HasPrefix(u.Path, "/") {
		return Target{}, fmt.Errorf("remote target %q must name an absolute path, got %q", raw, u.Path)
	}

	var user string
	if u.User != nil {
		user = u.User.Username()
	}

	return Target{
		User: user,
		Host: u.Hostname(),
		Port: u.Port(),
		Path: u.Path,
	}, nil
}

// String renders t back as the form ParseTarget accepts, for messages
// and logs.
func (t Target) String() string {
	host := t.Host
	if t.User != "" {
		host = t.User + "@" + host
	}
	if t.Port != "" {
		host = host + ":" + t.Port
	}
	return "ssh://" + host + t.Path
}

// HostString renders "user@host", for the UI header naming which machine
// a remote connection reaches -- unlike String(), it carries no scheme,
// port or path, since those don't belong on a header line next to a
// working directory.
func (t Target) HostString() string {
	if t.User != "" {
		return t.User + "@" + t.Host
	}
	return t.Host
}
