package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"mm-platform-engine/internal/config"
	"mm-platform-engine/internal/engine"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/exchange/gate"
	"mm-platform-engine/internal/exchange/mexc"
	"mm-platform-engine/internal/store"
)

func main() {
	log.Println("========================================")
	log.Println("    MM Engine - Market Making Bot")
	log.Println("========================================")

	// Load config from MongoDB
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	log.Printf("Loaded config for %s on %s", cfg.TradingConfig.Symbol, cfg.ExchangeName)

	// Create exchange client
	var exch exchange.Exchange
	exchangeName := strings.ToLower(cfg.ExchangeName)

	switch exchangeName {
	case "mexc":
		exch = mexc.NewClient(cfg.ExchangeAPIKey, cfg.ExchangeAPISecret, cfg.ExchangeBaseURL)
		log.Println("Using MEXC exchange")
	case "gate":
		gateSymbol := convertToGateSymbol(cfg.TradingConfig.Symbol)
		exch = gate.NewClient(cfg.ExchangeAPIKey, cfg.ExchangeAPISecret, cfg.ExchangeBaseURL, gateSymbol)
		log.Printf("Using Gate.io exchange for %s", gateSymbol)
	default:
		log.Fatalf("Unsupported exchange: %s", cfg.ExchangeName)
	}

	// Create Redis store
	redis, err := store.NewRedisStore(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	if err != nil {
		log.Fatalf("Failed to create Redis store: %v", err)
	}
	defer redis.Close()
	log.Println("Connected to Redis")

	// Create MongoDB store
	mongo, err := store.NewMongoStore(cfg.MongoURI, cfg.MongoDB)
	if err != nil {
		log.Fatalf("Failed to create MongoDB store: %v", err)
	}
	defer mongo.Close(context.Background())
	log.Println("Connected to MongoDB")

	// Convert trading config to engine config
	// Option 1: Use adapter for legacy config
	engineCfg := engine.AdaptConfig(&cfg.TradingConfig)

	// Option 2: Use default config with overrides
	// engineCfg := engine.DefaultConfig()
	// engineCfg.Symbol = cfg.TradingConfig.Symbol
	// engineCfg.BaseAsset = cfg.TradingConfig.BaseAsset
	// engineCfg.QuoteAsset = cfg.TradingConfig.QuoteAsset

	log.Printf("Engine config: symbol=%s, levels=%d, target_ratio=%.2f, base_spread=%.0f bps",
		engineCfg.Symbol,
		engineCfg.Depth.NumLevels,
		engineCfg.Inventory.TargetRatio,
		engineCfg.Spread.BaseSpreadBps,
	)

	// Create engine
	eng := engine.NewEngine(engineCfg, exch, redis, mongo)

	// Bot ID for identifying this bot instance in Redis stream
	botID := cfg.UserExchangeKeyID

	// Wire up Redis Stream for order events
	// NestJS gateway subscribes to mm:stream:{symbol}
	eng.SetOrderEventCallback(func(event engine.OrderEvent) {
		mmEvent := &store.MMOrderEvent{
			Type:      string(event.Type),
			Symbol:    event.Symbol,
			OrderID:   event.OrderID,
			Side:      event.Side,
			Price:     event.Price,
			Qty:       event.Qty,
			Level:     event.Level,
			Reason:    event.Reason,
			Timestamp: event.Timestamp,
			BotID:     botID,
		}
		if err := redis.PublishMMOrderEvent(context.Background(), mmEvent); err != nil {
			log.Printf("[REDIS] Failed to publish %s event: %v", event.Type, err)
		} else {
			log.Printf("[REDIS] Published %s %s L%d @ %.8f (id=%s, botID=%s)",
				event.Type, event.Side, event.Level, event.Price, event.OrderID, botID)
		}
	})
	log.Printf("Order events will be published to Redis stream mm:stream:%s (botID=%s)", engineCfg.Symbol, botID)

	// Setup context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start engine
	log.Println("Starting MM Engine...")
	if err := eng.Start(ctx); err != nil {
		log.Fatalf("Failed to start engine: %v", err)
	}

	// Update status in Redis
	if err := redis.SetStatus(ctx, engineCfg.Symbol, "running"); err != nil {
		log.Printf("Failed to set status: %v", err)
	}

	// Start monitoring goroutine
	go monitorLoop(ctx, eng)

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	log.Println("MM Engine is running. Press Ctrl+C to stop.")

	// Wait for signal
	sig := <-sigCh
	log.Printf("Received signal: %v", sig)

	// Graceful shutdown
	log.Println("Shutting down MM Engine...")

	// Create a channel to signal shutdown completion
	shutdownDone := make(chan struct{})

	go func() {
		// Update status
		if err := redis.SetStatus(context.Background(), engineCfg.Symbol, "stopped"); err != nil {
			log.Printf("Failed to set status: %v", err)
		}

		// Stop engine with timeout
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()

		if err := eng.Stop(shutdownCtx); err != nil {
			log.Printf("Error during shutdown: %v", err)
		}

		close(shutdownDone)
	}()

	// Wait for shutdown to complete or second signal for force quit
	select {
	case <-shutdownDone:
		log.Println("MM Engine stopped successfully")
	case sig := <-sigCh:
		log.Printf("Received second signal (%v), forcing immediate exit...", sig)
		// Force cancel all orders one more time before exit
		forceCtx, forceCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := eng.Stop(forceCtx); err != nil {
			log.Printf("Force shutdown error: %v", err)
		}
		forceCancel()
		log.Println("MM Engine force stopped")
	}
}

// monitorLoop periodically prints engine status
func monitorLoop(ctx context.Context, eng *engine.Engine) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			printStatus(eng)
		}
	}
}

// printStatus prints current engine status
func printStatus(eng *engine.Engine) {
	mode := eng.GetMode()
	metrics := eng.GetMetrics(1)

	fmt.Println()
	fmt.Println("============ MM ENGINE STATUS ============")
	fmt.Printf("Mode: %s\n", mode)

	if len(metrics) > 0 {
		m := metrics[0]
		fmt.Printf("Time: %s\n", m.Timestamp.Format("15:04:05"))
		fmt.Printf("Mid: %.6f\n", m.Mid)
		fmt.Printf("Spread: %.2f bps\n", m.AvgSpreadBps)
		fmt.Printf("Depth: $%.2f\n", m.DepthNotional)
		fmt.Printf("Orders: %d bids, %d asks\n", m.NumBidOrders, m.NumAskOrders)
		fmt.Printf("Inventory: %.2f%% (dev: %.2f%%)\n", m.InvRatio*100, m.InvDeviation*100)
		fmt.Printf("Skew: %.2f bps, Tilt: %.2f\n", m.SkewBps, m.SizeTilt)
		fmt.Printf("Drawdown 24h: %.2f%%\n", m.Drawdown24h*100)
		fmt.Printf("Fills/min: %.1f\n", m.FillsPerMin)
		fmt.Printf("Uptime: %.1f%%\n", m.QuoteUptime*100)
		fmt.Printf("NAV: $%.2f (Base: $%.2f, Quote: $%.2f)\n", m.NAV, m.BaseValue, m.QuoteValue)
	}

	fmt.Println("==========================================")
	fmt.Println()
}

// convertToGateSymbol converts symbol from "BTCUSDT" to "BTC_USDT" format
func convertToGateSymbol(symbol string) string {
	if strings.Contains(symbol, "_") {
		return symbol
	}
	quotes := []string{"USDT", "USDC", "BTC", "ETH", "USD"}
	for _, quote := range quotes {
		if strings.HasSuffix(symbol, quote) {
			base := strings.TrimSuffix(symbol, quote)
			return base + "_" + quote
		}
	}
	return symbol
}
