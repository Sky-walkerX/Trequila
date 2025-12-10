// Planner service - consumes trip requests and generates AI-powered itineraries
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/nats-io/nats.go"
	"github.com/sky-walkerx/trequila/internal/ai"
	"github.com/sky-walkerx/trequila/internal/ai/gemini"
	natshelper "github.com/sky-walkerx/trequila/internal/nats"
	"github.com/sky-walkerx/trequila/internal/planner"
)

func main() {
	// Load configuration from environment
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		log.Fatal("GEMINI_API_KEY environment variable is required")
	}

	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = nats.DefaultURL
	}

	// Create context with cancellation for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize Gemini AI generator
	aiConfig := ai.DefaultConfig(apiKey)
	generator, err := gemini.New(ctx, aiConfig)
	if err != nil {
		log.Fatalf("Failed to initialize Gemini: %v", err)
	}
	defer generator.Close()

	// Initialize NATS JetStream connection
	js := natshelper.ConnectJetStream(natsURL)

	// Create planner service with default configuration
	plannerConfig := planner.DefaultConfig()
	svc, err := planner.New(js, generator, plannerConfig)
	if err != nil {
		log.Fatalf("Failed to create planner service: %v", err)
	}
	defer svc.Stop()

	// Handle graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Received shutdown signal")
		cancel()
	}()

	// Start processing trip requests
	log.Println("Starting planner service...")
	if err := svc.Start(ctx, "TRIPS"); err != nil && err != context.Canceled {
		log.Fatalf("Planner service error: %v", err)
	}

	log.Println("Planner service stopped")
}
