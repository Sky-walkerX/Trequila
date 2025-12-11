package nats

import (
	"context"
	"log"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ConnectJetStream establishes a connection to NATS and initializes JetStream.
// It also ensures the TRIPS stream exists with proper configuration.
// Returns both the JetStream interface and the underlying NATS connection.
// 
// For Synadia NGS, pass credsFile path to authenticate with credentials file.
func ConnectJetStream(url string, credsFile ...string) (jetstream.JetStream, *nats.Conn) {
	// Build connection options
	opts := []nats.Option{
		nats.Name("Trequila Service"),     // Identifies this client in NATS
		nats.ReconnectWait(2*time.Second), // Wait 2s before reconnecting
		nats.MaxReconnects(-1),            // Unlimited reconnect attempts
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			log.Printf("NATS disconnected: %v", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Printf("NATS reconnected to %s", nc.ConnectedUrl())
		}),
	}

	// Add credentials file if provided (for Synadia NGS)
	if len(credsFile) > 0 && credsFile[0] != "" {
		log.Printf("Using NATS credentials file: %s", credsFile[0])
		opts = append(opts, nats.UserCredentials(credsFile[0]))
	}

	// 1. Connect to NATS server
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		log.Fatalf("NATS connect error: %v", err)
	}

	// 2. Create JetStream context from the NATS connection
	js, err := jetstream.New(nc)
	if err != nil {
		log.Fatalf("JetStream init: %v", err)
	}

	// 3. Ensure the TRIPS stream exists (idempotent - safe to call multiple times)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       "TRIPS",                // Stream name
		Subjects:   []string{"trips.*"},    // Subjects this stream captures
		Storage:    jetstream.FileStorage,  // Persistent storage (vs MemoryStorage)
		Retention:  jetstream.LimitsPolicy, // Retain based on limits (vs WorkQueuePolicy)
		MaxMsgs:    1000000,                // Max messages in stream
		MaxBytes:   1024 * 1024 * 1024,     // Max 1GB storage
		MaxAge:     7 * 24 * time.Hour,     // Keep messages for 7 days
		Duplicates: 2 * time.Minute,        // De-duplication window
		NoAck:      false,                  // Require acknowledgments
	})
	if err != nil {
		log.Fatalf("Failed to create/update stream: %v", err)
	}

	log.Printf("JetStream connected. Stream: %s, Subjects: %v", stream.CachedInfo().Config.Name, stream.CachedInfo().Config.Subjects)

	return js, nc
}
