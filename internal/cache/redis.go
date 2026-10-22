// Package cache builds the Redis client shared by the keyless enrichment services.
package cache

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"log"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
)

// TLSServerName returns the SNI/verification hostname for a rediss:// address.
// redis.Options.Addr is "host:port", which is never a valid certificate name, so
// the port has to be stripped. Managed providers (Heroku Data for Redis) present a
// certificate for a hostname that differs from the one in REDIS_URL, which makes
// verification fail on the name alone — REDIS_TLS_SERVER_NAME overrides it.
func TLSServerName(addr string) string {
	if override := os.Getenv("REDIS_TLS_SERVER_NAME"); override != "" {
		return override
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// RootCAs builds the trust store for a rediss:// connection from
// REDIS_TLS_CA_CERT, or returns nil to fall back to the system roots.
//
// Heroku Data for Redis signs with its own authority (`OU=Data Self Sign
// Authority`), which no system trust store carries, so the chain fails to verify
// however correct the hostname is. Pinning that authority here is what lets
// verification stay fully switched on: the certificate must still chain to this
// root *and* match the expected hostname.
//
// The value is a PEM bundle, optionally base64-encoded so it survives being set
// as a single-line config var. It holds no secret — the server presents these
// certificates to anyone who connects.
func RootCAs() *x509.CertPool {
	raw := strings.TrimSpace(os.Getenv("REDIS_TLS_CA_CERT"))
	if raw == "" {
		return nil
	}
	pem := []byte(raw)
	if !strings.Contains(raw, "-----BEGIN") {
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			log.Printf("⚠️  REDIS_TLS_CA_CERT is neither PEM nor valid base64; ignoring it: %v", err)
			return nil
		}
		pem = decoded
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		// Falling back to the system roots here would be a silent downgrade to a
		// connection that cannot verify, so say so loudly.
		log.Printf("⚠️  REDIS_TLS_CA_CERT contained no usable certificate; ignoring it")
		return nil
	}
	return pool
}

// InsecureSkipVerify reports whether REDIS_TLS_INSECURE_SKIP_VERIFY asks for
// certificate verification to be turned off.
//
// This is the last resort, not the fix: prefer REDIS_TLS_CA_CERT, which keeps
// verification on. Skipping it keeps the connection encrypted but stops
// authenticating the server, so an attacker positioned between the app and Redis
// would not be caught. It is opt-in and never inferred from the URL.
func InsecureSkipVerify() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("REDIS_TLS_INSECURE_SKIP_VERIFY"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// warnOnce keeps the startup warning to a single line however many services
// build a client.
var warnOnce sync.Once

// NewRedisClient builds a client from a redis:// or rediss:// URL, or nil if empty.
func NewRedisClient(redisURL string) *redis.Client {
	if redisURL == "" {
		return nil
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Printf("Failed to parse Redis URL, using as address: %v", err)
		return redis.NewClient(&redis.Options{Addr: redisURL})
	}
	if strings.HasPrefix(redisURL, "rediss://") {
		// ServerName is always set: it is the SNI the server selects its
		// certificate by, not only what gets verified against.
		cfg := &tls.Config{ServerName: TLSServerName(opt.Addr)}
		cfg.RootCAs = RootCAs()

		if InsecureSkipVerify() {
			cfg.InsecureSkipVerify = true
			warnOnce.Do(func() {
				log.Printf("⚠️  Redis TLS certificate verification is DISABLED (REDIS_TLS_INSECURE_SKIP_VERIFY). " +
					"Traffic is encrypted, but the server's identity is not checked. " +
					"Prefer REDIS_TLS_CA_CERT, which keeps verification on.")
			})
		} else if cfg.RootCAs != nil {
			warnOnce.Do(func() {
				log.Printf("Redis TLS: verifying against the pinned authority in REDIS_TLS_CA_CERT (server name %q)", cfg.ServerName)
			})
		}
		opt.TLSConfig = cfg
	}
	return redis.NewClient(opt)
}
