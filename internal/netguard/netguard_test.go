package netguard

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestBlockedIP(t *testing.T) {
	cases := map[string]struct {
		ip    string
		block bool
	}{
		"loopback":    {"127.0.0.1", true},
		"loopback v6": {"::1", true},
		"private 10":  {"10.0.0.1", true},
		"private 192": {"192.168.1.1", true},
		"private 172": {"172.16.0.1", true},
		"link-local":  {"169.254.1.1", true},
		"unspecified": {"0.0.0.0", true},
		"public":      {"8.8.8.8", false},
		"public v6":   {"2001:4860:4860::8888", false},
	}
	for name, c := range cases {
		if got := BlockedIP(net.ParseIP(c.ip)); got != c.block {
			t.Errorf("%s: BlockedIP(%s) = %v, want %v", name, c.ip, got, c.block)
		}
	}
}

func TestPrivateHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cases := []struct {
		host  string
		block bool
	}{
		{"127.0.0.1", true},
		{"10.1.2.3", true},
		{"192.168.0.10", true},
		{"8.8.8.8", false},
		// Malformed and unresolvable inputs fail closed: a caller must never
		// fetch a host it could not positively verify as public.
		{"", true},
		{"not a host!", true},
		{"definitely-not-a-real-host-zz.invalid", true},
	}
	for _, c := range cases {
		if got := PrivateHost(ctx, c.host); got != c.block {
			t.Errorf("PrivateHost(%q) = %v, want %v", c.host, got, c.block)
		}
	}
}

func TestRestrictedDialContextRefusesBlocked(t *testing.T) {
	dial := RestrictedDialContext(&net.Dialer{Timeout: time.Second})
	for _, addr := range []string{"127.0.0.1:80", "169.254.169.254:80", "10.0.0.1:80"} {
		if conn, err := dial(context.Background(), "tcp", addr); !errors.Is(err, ErrBlocked) {
			if conn != nil {
				conn.Close()
			}
			t.Errorf("dial %s err = %v, want ErrBlocked", addr, err)
		}
	}
}
