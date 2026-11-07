// Package worker configures the raw TCP-over-WebSocket Worker transport.
package worker

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"tgws/internal/ws"
)

var hostname = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)

// Domain accepts a hostname, not an arbitrary URL, credentials or request path.
func Domain(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", nil
	}
	if len(s) > 253 || !hostname.MatchString(s) {
		return "", errors.New("worker-domain: expected a DNS hostname without scheme, path or port")
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return "", errors.New("worker-domain: use a hostname with a valid TLS certificate")
	}
	return s, nil
}

func ReadToken(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("worker-token-file: cannot open file")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 258))
	if err != nil {
		return "", errors.New("worker-token-file: cannot read file")
	}
	s := strings.TrimSpace(string(b))
	if len(b) >= 258 || !tokenPattern.MatchString(s) {
		return "", errors.New("worker-token-file: expected 32–256 letters, digits, underscores or dashes")
	}
	return s, nil
}

// Dialer preserves the original Telegram IP and port. The supplied Worker
// validates both. Flowseal's original Worker ignores port and always uses 443.
func Dialer(domain, token string, dst netip.AddrPort, mark int) ws.Dialer {
	return ws.Dialer{
		Addr: net.JoinHostPort(domain, "443"), ServerName: domain,
		Path:        "/apiws?dst=" + dst.Addr().String() + "&port=" + strconv.Itoa(int(dst.Port())),
		BearerToken: token, Mark: mark, Timeout: 5 * time.Second, ReadTimeout: 5 * time.Minute,
	}
}
