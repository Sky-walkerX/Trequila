package nats

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ConnectJetStream establishes a connection to NATS and initializes JetStream.
// Supports two auth modes:
//   - credsFile: path to .creds file (local dev)
//   - credsContent: raw credentials content (Heroku - stored in env var)
func ConnectJetStream(url, credsFile, credsContent string) (jetstream.JetStream, *nats.Conn) {
	opts := []nats.Option{
		nats.Name("Trequila Service"),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			log.Printf("NATS disconnected: %v", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Printf("NATS reconnected to %s", nc.ConnectedUrl())
		}),
	}

	// Priority: inline content > file path
	if credsContent != "" {
		log.Println("Using inline NATS credentials (NATS_CREDS)")
		tmpFile, err := os.CreateTemp("", "nats-*.creds")
		if err != nil {
			log.Fatalf("Failed to create temp creds file: %v", err)
		}
		if _, err := tmpFile.WriteString(credsContent); err != nil {
			log.Fatalf("Failed to write creds: %v", err)
		}
		tmpFile.Close()
		opts = append(opts, nats.UserCredentials(tmpFile.Name()))
	} else if credsFile != "" {
		log.Printf("Using NATS credentials file: %s", credsFile)
		opts = append(opts, nats.UserCredentials(credsFile))
	}

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
