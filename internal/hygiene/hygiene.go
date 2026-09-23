// Package hygiene checks that files published in this repository carry no
// private operational detail: no IP address literals, no home-directory
// paths, and none of the entries in an externally supplied deny-list.
//
// The deny-list is deliberately not part of this repository. Maintainers
// supply it at build time; publishing it would disclose what it protects.
package hygiene

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strings"
)

// Finding is one violation in one file.
type Finding struct {
	Path string
	Line int
	Rule string
	// Match is the offending text. For deny-list hits it is withheld, so
	// that the report never repeats a private value.
	Match string
}

func (f Finding) String() string {
	if f.Match == "" {
		return fmt.Sprintf("%s:%d: %s", f.Path, f.Line, f.Rule)
	}
	return fmt.Sprintf("%s:%d: %s: %q", f.Path, f.Line, f.Rule, f.Match)
}

var (
	ipv4Pattern = regexp.MustCompile(`(?:^|[^0-9A-Za-z.])((?:[0-9]{1,3}\.){3}[0-9]{1,3})(?:[^0-9A-Za-z.]|$)`)
	homePattern = regexp.MustCompile(`/(?:Users|home)/[A-Za-z0-9._-]+`)
)

// allowedIPv4 are addresses that disclose nothing: loopback, the unspecified
// address, broadcast, and the RFC 5737 documentation ranges used in fixtures.
var allowedIPv4 = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("0.0.0.0/32"),
	netip.MustParsePrefix("255.255.255.255/32"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
}

// Checker holds a parsed deny-list.
type Checker struct {
	deny []string // lower-cased
}

// ParseDenyList reads one case-insensitive substring per line. Blank lines
// and lines starting with '#' are ignored.
func ParseDenyList(r io.Reader) (*Checker, error) {
	c := &Checker{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		c.deny = append(c.deny, strings.ToLower(line))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return c, nil
}

// Entries reports how many deny-list entries are loaded.
func (c *Checker) Entries() int { return len(c.deny) }

// CheckFile scans one file's content. Binary content (a NUL byte in the
// first 8 KiB) is skipped.
func (c *Checker) CheckFile(path string, content []byte) []Finding {
	head := content
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return nil
	}
	var out []Finding
	lowerPath := strings.ToLower(path)
	for _, d := range c.deny {
		if strings.Contains(lowerPath, d) {
			out = append(out, Finding{Path: path, Line: 0, Rule: "deny-list entry in file path"})
		}
	}
	for i, line := range strings.Split(string(content), "\n") {
		n := i + 1
		for _, m := range ipv4Pattern.FindAllStringSubmatch(line, -1) {
			if addr, err := netip.ParseAddr(m[1]); err == nil && !isAllowed(addr) {
				out = append(out, Finding{Path: path, Line: n, Rule: "IPv4 literal", Match: m[1]})
			}
		}
		for _, m := range homePattern.FindAllString(line, -1) {
			out = append(out, Finding{Path: path, Line: n, Rule: "home-directory path", Match: m})
		}
		lower := strings.ToLower(line)
		for _, d := range c.deny {
			if strings.Contains(lower, d) {
				out = append(out, Finding{Path: path, Line: n, Rule: "deny-list entry"})
			}
		}
	}
	return out
}

func isAllowed(a netip.Addr) bool {
	for _, p := range allowedIPv4 {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
