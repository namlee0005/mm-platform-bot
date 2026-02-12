package bot

import (
	"context"
	"log"
	"time"
)

// shutdown performs a graceful shutdown of all bot components
func (b *Bot) shutdown(ctx context.Context) error {
	log.Println("Initiating graceful shutdown...")

	// Create a timeout context for shutdown operations
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Cancel all orders on the exchange before shutdown
	symbol := b.cfg.TradingConfig.Symbol
	log.Printf("Cancelling all orders for %s...", symbol)
	if err := b.exchange.CancelAllOrders(shutdownCtx, symbol); err != nil {
		log.Printf("Warning: Failed to cancel all orders: %v", err)
	} else {
		log.Printf("Successfully cancelled all orders")
	}

	// Clear all orders from Redis
	log.Printf("Clearing orders from Redis...")
	if err := b.redis.ClearAllOrders(shutdownCtx, symbol); err != nil {
		log.Printf("Warning: Failed to clear orders from Redis: %v", err)
	} else {
		log.Printf("Successfully cleared orders from Redis")
	}

	// Stop HTTP server
	if err := b.http.Stop(shutdownCtx); err != nil {
		log.Printf("Warning: Failed to stop HTTP server: %v", err)
	}

	// Stop exchange client
	if err := b.exchange.Stop(shutdownCtx); err != nil {
		log.Printf("Warning: Failed to stop exchange client: %v", err)
	}

	// Close Redis connection
	if err := b.redis.Close(); err != nil {
		log.Printf("Warning: Failed to close Redis connection: %v", err)
	}

	// Close MongoDB connection
	if err := b.mongo.Close(shutdownCtx); err != nil {
		log.Printf("Warning: Failed to close MongoDB connection: %v", err)
	}

	log.Println("Graceful shutdown completed")
	return nil
}

// emergencyShutdown performs immediate shutdown without a cleanup
func (b *Bot) emergencyShutdown() {
	log.Println("Emergency shutdown initiated!")

	if b.cancel != nil {
		b.cancel()
	}

	b.mu.Lock()
	b.running = false
	b.mu.Unlock()

	log.Println("Emergency shutdown completed")
}
