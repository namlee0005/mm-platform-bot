package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"mm-platform-engine/internal/bot"
	"mm-platform-engine/internal/config"
)

func main() {

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Create bot instance
	b, err := bot.NewBot(cfg)
	if err != nil {
		log.Fatalf("Failed to create bot: %v", err)
	}

	// Setup signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	// Start bot
	if err := b.Start(ctx); err != nil {
		log.Fatalf("Failed to start bot: %v", err)
	}

	log.Println("Bot started successfully. Waiting for shutdown signal...")

	// Wait for shutdown signal
	<-sigChan
	log.Println("Shutdown signal received, stopping bot...")

	// Graceful shutdown
	if err := b.Stop(ctx); err != nil {
		log.Printf("Error during shutdown: %v", err)
	}

	log.Println("Bot stopped successfully")
}
