package main

import (
	"context"
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
	log.Println("    Simple Maker Bot")
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

	// Create MongoDB store for config updates
	mongo, err := store.NewMongoStore(cfg.MongoURI, cfg.MongoDB)
	if err != nil {
		log.Fatalf("Failed to create MongoDB store: %v", err)
	}
	defer mongo.Close(context.Background())
	log.Println("Connected to MongoDB for config updates")

	// Bot ID for identifying this instance
	botID := cfg.UserExchangeKeyID

	// Get bot_type from MongoDB config (FIXED side - không switch)
	botType := cfg.SimpleConfig.BotType
	if botType == "" {
		log.Fatal("bot_type is required in config (maker-bid or maker-ask)")
	}

	var botSide engine.BotSide
	switch strings.ToLower(botType) {
	case "maker-bid", "bid":
		botSide = engine.BotSideBid
		log.Println("Mode: MAKER-BID (BUY orders only) - FIXED")
	case "maker-ask", "ask":
		botSide = engine.BotSideAsk
		log.Println("Mode: MAKER-ASK (SELL orders only) - FIXED")
	default:
		log.Fatalf("Invalid bot_type: %s (must be 'maker-bid' or 'maker-ask')", botType)
	}

	// Create simple maker config from loaded config
	simpleConfig := cfg.SimpleConfig
	makerCfg := &engine.SimpleMakerConfig{
		Symbol:              simpleConfig.Symbol,
		BaseAsset:           simpleConfig.BaseAsset,
		QuoteAsset:          simpleConfig.QuoteAsset,
		BotSide:             botSide,
		SpreadBps:           simpleConfig.SpreadMinBps,
		NumLevels:           simpleConfig.NumLevels,
		TargetDepthNotional: simpleConfig.TargetDepthNotional,
		TickIntervalMs:      simpleConfig.TickIntervalMs,
		LadderRegenBps:      simpleConfig.LadderRegenBps,
		MinBalanceToTrade:   simpleConfig.MinBalanceToTrade,
		LevelGapTicksMax:    simpleConfig.LevelGapTicksMax,
		DepthBps:            simpleConfig.DepthBps,
		Exchange:            exchangeName,
		BotID:               botID,
	}

	// Set defaults if not configured
	if makerCfg.SpreadBps == 0 {
		makerCfg.SpreadBps = 50 // 0.5%
	}
	if makerCfg.NumLevels == 0 {
		makerCfg.NumLevels = 5
	}
	if makerCfg.TargetDepthNotional == 0 {
		makerCfg.TargetDepthNotional = 1000 // $1000 default
	}
	if makerCfg.TickIntervalMs == 0 {
		makerCfg.TickIntervalMs = 5000
	}
	if makerCfg.LadderRegenBps == 0 {
		makerCfg.LadderRegenBps = 50 // 0.5% default
	}
	if makerCfg.LevelGapTicksMax == 0 {
		makerCfg.LevelGapTicksMax = 3 // default: 1-3 ticks random gap
	}
	if makerCfg.DepthBps == 0 {
		makerCfg.DepthBps = 200 // default: 200 bps = ±2% from mid
	}

	log.Printf("Simple Maker config: side=%s, spread=%.0fbps, levels=%d, gapTicks=1-%d, depth=$%.0f, depthBps=%.0f, regenBps=%.0f",
		makerCfg.BotSide, makerCfg.SpreadBps, makerCfg.NumLevels, makerCfg.LevelGapTicksMax, makerCfg.TargetDepthNotional, makerCfg.DepthBps, makerCfg.LadderRegenBps)

	// Create simple maker
	maker := engine.NewSimpleMaker(makerCfg, exch, redis, mongo)

	// Wire up Redis Stream for order events
	maker.SetOrderEventCallback(func(event engine.OrderEvent) {
		log.Printf("[REDIS] Publishing %s event for order %s", event.Type, event.OrderID)
		mmEvent := &store.MMOrderEvent{
			Type:      string(event.Type),
			Exchange:  exchangeName,
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
			log.Printf("[REDIS] Published %s event to mm:stream:%s:%s", event.Type, exchangeName, event.Symbol)
		}
	})
	log.Printf("Order events will be published to Redis stream mm:stream:%s:%s (botID=%s)", exchangeName, makerCfg.Symbol, botID)

	// Setup context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start maker
	if err := maker.Start(ctx); err != nil {
		log.Fatalf("Failed to start maker: %v", err)
	}

	// Update status in Redis
	statusKey := string(botSide) // "bid" or "ask"
	if err := redis.SetStatus(ctx, makerCfg.Symbol+":"+statusKey, "running"); err != nil {
		log.Printf("Failed to set status: %v", err)
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	log.Printf("Simple Maker %s is running. Press Ctrl+C to stop.", botSide)

	// Wait for first signal
	sig := <-sigCh
	log.Printf("Received signal: %v", sig)
	log.Println("Shutting down... (please wait for orders to be cancelled)")

	// Ignore further signals during shutdown
	signal.Stop(sigCh)

	// Update status
	if err := redis.SetStatus(context.Background(), makerCfg.Symbol+":"+statusKey, "stopped"); err != nil {
		log.Printf("Failed to set status: %v", err)
	}

	// Stop maker with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := maker.Stop(shutdownCtx); err != nil {
		log.Printf("Error during shutdown: %v", err)
	}

	log.Println("Simple Maker stopped successfully")
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
