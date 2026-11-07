package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDomainValidation(t *testing.T) {
	for _, s := range []string{"a.example.workers.dev", "proxy.example.com", ""} {
		if _, err := Domain(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{"https://a.workers.dev", "a.workers.dev/path", "a.workers.dev:443", "user@host.dev", "a.dev\r\nAuthorization: x", "127.0.0.1", "-bad.dev", "a..dev"} {
		if _, err := Domain(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestTokenValidationAndRedaction(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token")
	good := strings.Repeat("a", 64)
	if err := os.WriteFile(p, []byte(good+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadToken(p); err != nil || got != good {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"secret", good + "\r\nInjected: secret", strings.Repeat("a", 259)} {
		if err := os.WriteFile(p, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadToken(p)
		if err == nil || strings.Contains(err.Error(), bad) {
			t.Fatalf("unsafe error: %v", err)
		}
	}
}
