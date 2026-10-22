package cache

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func TestTLSServerNameStripsPort(t *testing.T) {
	t.Setenv("REDIS_TLS_SERVER_NAME", "")

	// redis.Options.Addr is host:port; passing it through verbatim makes every
	// certificate check fail on the name.
	if got := TLSServerName("ec2-34-195-9-12.compute-1.amazonaws.com:22490"); got != "ec2-34-195-9-12.compute-1.amazonaws.com" {
		t.Errorf("expected the bare host, got %q", got)
	}
	if got := TLSServerName("localhost"); got != "localhost" {
		t.Errorf("expected a portless address to pass through, got %q", got)
	}
}

func TestTLSServerNameHonoursOverride(t *testing.T) {
	t.Setenv("REDIS_TLS_SERVER_NAME", "dk2micb3lhib7.service.shogun.heroku.com")

	if got := TLSServerName("ec2-34-195-9-12.compute-1.amazonaws.com:22490"); got != "dk2micb3lhib7.service.shogun.heroku.com" {
		t.Errorf("expected the override, got %q", got)
	}
}

func TestNewRedisClientEmptyURL(t *testing.T) {
	if c := NewRedisClient(""); c != nil {
		t.Errorf("expected nil client for an empty URL, got %v", c)
	}
}

// Turning off verification is a security decision, so it must be explicit:
// nothing about the URL or the provider may switch it on by itself.
func TestInsecureSkipVerifyIsOffUnlessAskedFor(t *testing.T) {
	for _, unset := range []string{"", "0", "false", "no", "off", "maybe", "TRUE-ish"} {
		t.Setenv("REDIS_TLS_INSECURE_SKIP_VERIFY", unset)
		if InsecureSkipVerify() {
			t.Errorf("%q should not disable verification", unset)
		}
	}
	for _, set := range []string{"1", "true", "TRUE", " yes ", "Yes"} {
		t.Setenv("REDIS_TLS_INSECURE_SKIP_VERIFY", set)
		if !InsecureSkipVerify() {
			t.Errorf("%q should disable verification", set)
		}
	}
}

func TestRedissClientVerifiesByDefault(t *testing.T) {
	t.Setenv("REDIS_TLS_INSECURE_SKIP_VERIFY", "")
	t.Setenv("REDIS_TLS_SERVER_NAME", "")

	c := NewRedisClient("rediss://:pw@example.host:6380")
	tlsCfg := c.Options().TLSConfig
	if tlsCfg == nil {
		t.Fatal("a rediss:// URL must configure TLS")
	}
	if tlsCfg.InsecureSkipVerify {
		t.Error("verification must stay on unless explicitly disabled")
	}
	if tlsCfg.ServerName != "example.host" {
		t.Errorf("ServerName = %q, want the bare host", tlsCfg.ServerName)
	}
}

func TestRedissClientHonoursInsecureOptIn(t *testing.T) {
	t.Setenv("REDIS_TLS_INSECURE_SKIP_VERIFY", "true")
	t.Setenv("REDIS_TLS_SERVER_NAME", "dk2micb3lhib7.service.shogun.heroku.com")

	tlsCfg := NewRedisClient("rediss://:pw@ec2-34-195-9-12.compute-1.amazonaws.com:22490").Options().TLSConfig
	if !tlsCfg.InsecureSkipVerify {
		t.Error("expected verification to be disabled")
	}
	// SNI still matters with verification off: it is how the server picks which
	// certificate to present.
	if tlsCfg.ServerName != "dk2micb3lhib7.service.shogun.heroku.com" {
		t.Errorf("ServerName = %q, want the override to still apply", tlsCfg.ServerName)
	}
}

// Plain redis:// is not TLS at all; it must not pick up a TLS config.
func TestPlainRedisURLGetsNoTLS(t *testing.T) {
	t.Setenv("REDIS_TLS_INSECURE_SKIP_VERIFY", "true")

	if tlsCfg := NewRedisClient("redis://localhost:6379").Options().TLSConfig; tlsCfg != nil {
		t.Errorf("redis:// should not configure TLS, got %+v", tlsCfg)
	}
}

// --- CA pinning ---------------------------------------------------------
//
// These drive a real TLS handshake against a throwaway server whose certificate
// is signed by a throwaway authority, mirroring the shape of the Heroku chain: a
// self-signed root that no system trust store carries. Asserting on the
// tls.Config alone would not show whether verification actually succeeds.

// selfSignedCA returns a PEM-encoded CA and a leaf certificate it signed for host.
func selfSignedCA(t *testing.T, host string) (caPEM []byte, leaf tls.Certificate) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test.self.signed"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return caPEM, tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
}

// serveTLS starts a TLS listener and returns its address.
func serveTLS(t *testing.T, leaf tls.Certificate) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{leaf}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

// dial performs a handshake with the config NewRedisClient would have produced.
func dial(t *testing.T, addr string, cfg *tls.Config) error {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func TestPinnedCAVerifiesWithoutDisablingTLS(t *testing.T) {
	const host = "redis.internal.test"
	caPEM, leaf := selfSignedCA(t, host)
	addr := serveTLS(t, leaf)

	t.Setenv("REDIS_TLS_CA_CERT", string(caPEM))
	t.Setenv("REDIS_TLS_SERVER_NAME", host)
	t.Setenv("REDIS_TLS_INSECURE_SKIP_VERIFY", "")

	cfg := NewRedisClient("rediss://:pw@" + addr).Options().TLSConfig
	if cfg.InsecureSkipVerify {
		t.Fatal("pinning a CA must not disable verification")
	}
	if cfg.RootCAs == nil {
		t.Fatal("expected the pinned CA to become the trust store")
	}
	if err := dial(t, addr, cfg); err != nil {
		t.Fatalf("handshake against the pinned CA failed: %v", err)
	}
}

// Without the pin the same server is correctly rejected — proof the pin is what
// carries the handshake, rather than the test server being trusted anyway.
func TestSelfSignedServerIsRejectedWithoutThePin(t *testing.T) {
	const host = "redis.internal.test"
	_, leaf := selfSignedCA(t, host)
	addr := serveTLS(t, leaf)

	t.Setenv("REDIS_TLS_CA_CERT", "")
	t.Setenv("REDIS_TLS_SERVER_NAME", host)
	t.Setenv("REDIS_TLS_INSECURE_SKIP_VERIFY", "")

	cfg := NewRedisClient("rediss://:pw@" + addr).Options().TLSConfig
	if err := dial(t, addr, cfg); err == nil {
		t.Fatal("an unpinned self-signed certificate should not verify")
	}
}

// Pinning must not become a hostname bypass: a certificate from the pinned CA
// issued for a different host still has to fail.
func TestPinnedCAStillChecksTheHostname(t *testing.T) {
	caPEM, leaf := selfSignedCA(t, "redis.internal.test")
	addr := serveTLS(t, leaf)

	t.Setenv("REDIS_TLS_CA_CERT", string(caPEM))
	t.Setenv("REDIS_TLS_SERVER_NAME", "someone.else.test")
	t.Setenv("REDIS_TLS_INSECURE_SKIP_VERIFY", "")

	cfg := NewRedisClient("rediss://:pw@" + addr).Options().TLSConfig
	if err := dial(t, addr, cfg); err == nil {
		t.Fatal("a mismatched hostname should still fail under a pinned CA")
	}
}

func TestPinnedCAAcceptsBase64ForSingleLineConfigVars(t *testing.T) {
	const host = "redis.internal.test"
	caPEM, leaf := selfSignedCA(t, host)
	addr := serveTLS(t, leaf)

	t.Setenv("REDIS_TLS_CA_CERT", base64.StdEncoding.EncodeToString(caPEM))
	t.Setenv("REDIS_TLS_SERVER_NAME", host)
	t.Setenv("REDIS_TLS_INSECURE_SKIP_VERIFY", "")

	cfg := NewRedisClient("rediss://:pw@" + addr).Options().TLSConfig
	if cfg.RootCAs == nil {
		t.Fatal("a base64-encoded bundle should be decoded")
	}
	if err := dial(t, addr, cfg); err != nil {
		t.Fatalf("handshake with a base64 pin failed: %v", err)
	}
}

// Garbage must not silently fall back to a trust store that cannot verify.
func TestUnusableCACertIsIgnoredNotFatal(t *testing.T) {
	for name, value := range map[string]string{
		"not base64 or PEM": "%%%not-a-cert%%%",
		"base64 of garbage": base64.StdEncoding.EncodeToString([]byte("nope")),
		"empty PEM block":   "-----BEGIN CERTIFICATE-----\n-----END CERTIFICATE-----",
	} {
		t.Setenv("REDIS_TLS_CA_CERT", value)
		if pool := RootCAs(); pool != nil {
			t.Errorf("%s should not produce a trust store", name)
		}
	}
}

func TestNoCACertLeavesSystemRoots(t *testing.T) {
	t.Setenv("REDIS_TLS_CA_CERT", "")
	if pool := RootCAs(); pool != nil {
		t.Error("expected nil so crypto/tls uses the system roots")
	}
}
