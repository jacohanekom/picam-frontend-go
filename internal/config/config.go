// Package config loads picam-frontend's INI-style configuration file.
//
// Same format as the sibling picam-orchestrator project's config:
// "[section]" headers, "key = value" pairs (value is everything after the
// first '=' up to the first unquoted ';' or '#', trimmed), blank lines and
// full-line comments ignored.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type entry struct {
	section, key, value string
}

// rawStore is the parsed file: a flat "section.key" -> value map for
// ordinary lookups, plus the entries in file order for [pis] (which has
// an arbitrary, unknown-in-advance number of keys — one per configured Pi).
type rawStore struct {
	byKey   map[string]string
	entries []entry
}

func parseFile(path string) (*rawStore, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open config: %w", err)
	}
	defer f.Close()

	store := &rawStore{byKey: map[string]string{}}
	section := ""
	scanner := bufio.NewScanner(f)
	lineno := 0
	for scanner.Scan() {
		lineno++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] == ';' || line[0] == '#' {
			continue
		}

		if line[0] == '[' {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				return nil, fmt.Errorf("%s:%d: unclosed '['", path, lineno)
			}
			section = strings.TrimSpace(line[1:end])
			continue
		}

		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(stripComment(line[eq+1:]))
		if key == "" {
			continue
		}

		store.entries = append(store.entries, entry{section, key, val})
		fullKey := key
		if section != "" {
			fullKey = section + "." + key
		}
		store.byKey[fullKey] = val
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return store, nil
}

// stripComment removes a trailing ';' or '#' comment, ignoring either
// character while inside a double-quoted span (INI values here are
// normally unquoted, so this only matters for the rare quoted value).
func stripComment(s string) string {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case ';', '#':
			if !inQuote {
				return s[:i]
			}
		}
	}
	return s
}

func (r *rawStore) str(key, def string) string {
	if v, ok := r.byKey[key]; ok {
		return v
	}
	return def
}

func (r *rawStore) int(key string, def int) int {
	v, ok := r.byKey[key]
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// section returns every key/value pair under [name], in file order.
func (r *rawStore) section(name string) []entry {
	var out []entry
	for _, e := range r.entries {
		if e.section == name {
			out = append(out, e)
		}
	}
	return out
}

// Backend is one configured picam-orchestrator instance this frontend can
// show: a short name (used in ?pi=NAME and internally), a display label,
// and the host:port to reach it.
type Backend struct {
	Name  string
	Label string
	Host  string
	Port  int
}

// Config holds every setting picam-frontend needs, parsed once at startup.
type Config struct {
	// [pis]
	Backends []Backend

	// [output]
	HTTPPort int
	WebDir   string

	// [webrtc]
	ICEPortMin int
	ICEPortMax int
}

// Load reads and parses path, applying the same defaults the C++
// implementation falls back to when a key is entirely absent.
func Load(path string) (*Config, error) {
	r, err := parseFile(path)
	if err != nil {
		return nil, err
	}

	var backends []Backend
	for _, e := range r.section("pis") {
		b := Backend{Name: e.key, Port: 81}

		rest := e.value
		hostPort := rest
		if comma := strings.IndexByte(rest, ','); comma >= 0 {
			hostPort = rest[:comma]
			b.Label = strings.TrimSpace(rest[comma+1:])
		}
		if b.Label == "" {
			b.Label = b.Name
		}

		if colon := strings.IndexByte(hostPort, ':'); colon >= 0 {
			b.Host = hostPort[:colon]
			if p, err := strconv.Atoi(hostPort[colon+1:]); err == nil {
				b.Port = p
			}
		} else {
			// No :port — 81 is picam-orchestrator's own default port,
			// not picam-frontend's; an unqualified [pis] entry assumes
			// the backend is running with its own default.
			b.Host = hostPort
		}

		backends = append(backends, b)
	}

	return &Config{
		Backends: backends,

		HTTPPort: r.int("output.http_port", 80),
		WebDir:   r.str("output.web_dir", "/usr/share/picam-frontend/web"),

		ICEPortMin: r.int("webrtc.ice_port_min", 50000),
		ICEPortMax: r.int("webrtc.ice_port_max", 50200),
	}, nil
}
