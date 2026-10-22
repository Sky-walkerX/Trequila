package netguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestBlockedAddresses(t *testing.T) {
	blockedAddrs := []string{
		"127.0.0.1", "127.8.9.10", "::1",
		"10.0.0.5", "172.16.0.1", "172.31.255.255", "192.168.1.1", "fd00::1",
		"169.254.169.254", "169.254.0.1", "fe80::1",
		"100.64.0.1", "0.0.0.0", "::", "224.0.0.1", "ff02::1", "240.0.0.1", "198.18.0.1",
		"::ffff:127.0.0.1", "::ffff:10.1.2.3", "::ffff:169.254.169.254",
		// IPv6 transition mechanisms that embed or route to an IPv4 address.
		"64:ff9b::7f00:1", "64:ff9b::a00:1", "64:ff9b:1::1", // NAT64
		"2002:7f00:1::1", "2002:a9fe:a9fe::1", // 6to4
		"2001:0:4136:e378:8000:63bf:3fff:fdd2", // Teredo
		"100::1", "fec0::1", "192.88.99.1", "2001:db8::1",
	}
	for _, s := range blockedAddrs {
		if !blocked(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "172.32.0.1", "2606:4700:4700::1111", "2a00:1450:4001::1", "::ffff:8.8.8.8"}
	for _, s := range allowed {
		if blocked(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestValidateBaseURL(t *testing.T) {
	good := []string{"https://api.openai.com/v1", "https://llm.example.com", "https://8.8.8.8/v1"}
	for _, u := range good {
		if _, err := ValidateBaseURL(u, false); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := map[string]string{
		"plain http":       "http://api.example.com/v1",
		"no scheme":        "api.example.com/v1",
		"ftp":              "ftp://example.com",
		"user info":        "https://user:pass@example.com",
		"empty host":       "https:///v1",
		"loopback literal": "https://127.0.0.1:8080",
		"private literal":  "https://10.0.0.1",
		"metadata literal": "https://169.254.169.254/latest/meta-data",
		"ipv6 loopback":    "https://[::1]/v1",
		"localhost":        "https://localhost:11434",
		"dot localhost":    "https://foo.localhost",
		"internal suffix":  "https://metadata.google.internal",
		"empty":            "",
	}
	for name, u := range bad {
		if _, err := ValidateBaseURL(u, false); err == nil {
			t.Errorf("%s (%s) should be rejected", name, u)
		}
	}
}

func TestValidateBaseURLAllowsPrivateTargetsWhenTheOperatorOptsIn(t *testing.T) {
	for _, u := range []string{"http://localhost:11434/v1", "https://10.0.0.5/v1", "http://llm.internal:8000"} {
		if _, err := ValidateBaseURL(u, true); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	if _, err := ValidateBaseURL("ftp://x", true); err == nil {
		t.Error("opting in to private hosts must not allow other schemes")
	}
}

func get(t *testing.T, c *http.Client, url string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func TestTheDialGuardRefusesLoopbackEvenForAHostnameAndAllowsItWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()

	guarded := &http.Client{Transport: Transport(false)}
	err := get(t, guarded, srv.URL)
	if err == nil || !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("connect to loopback = %v, want ErrBlockedAddress", err)
	}

	// A hostname that resolves to loopback is caught at connect time too, which is what
	// defeats DNS rebinding: the check is on the address actually dialled.
	port := srv.URL[len("http://127.0.0.1"):]
	if err := get(t, guarded, "http://localhost"+port); err == nil || !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("connect via localhost = %v, want ErrBlockedAddress", err)
	}

	open := &http.Client{Transport: Transport(true)}
	if err := get(t, open, srv.URL); err != nil {
		t.Fatalf("opted-in client could not connect: %v", err)
	}
}
