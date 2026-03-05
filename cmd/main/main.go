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

	"mm-platform-engine/internal/bot"
	"mm-platform-engine/internal/config"
	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/exchange/gate"
	"mm-platform-engine/internal/exchange/mexc"
	"mm-platform-engine/internal/store"
)

func main() {
	log.Println("========================================")
	log.Println("    Unified Market Making Bot")
	log.Println("========================================")

	// Load config from MongoDB
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Get bot type from config (can be overridden by env)
	botType := os.Getenv("BOT_TYPE")
	if botType == "" {
		botType = cfg.SimpleConfig.BotType
	}
	if botType == "" {
		botType = "simple-maker" // Default to simple-maker (2-sided)
	}

	log.Printf("Loaded config for %s on %s, bot_type=%s",
		cfg.TradingConfig.Symbol, cfg.ExchangeName, botType)

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

	// Bot ID
	botID := cfg.UserExchangeKeyID

	// Create bot based on type
	var maker *core.BaseBot

	switch strings.ToLower(botType) {
	case "simple-maker":
		// All simple/one-sided types now use 2-sided simple-maker
		maker = createSimpleMaker(cfg, exch, redis, mongo, exchangeName, botID)
		log.Println("Mode: SIMPLE-MAKER (2-sided market maker)")

	case "depth-filler":
		maker = createDepthFiller(cfg, exch, redis, mongo, exchangeName, botID)
		log.Println("Mode: DEPTH-FILLER (order book depth filler)")

	default:
		log.Fatalf("Invalid bot_type: %s (must be 'simple-maker' or 'depth-filler')", botType)
	}

	// Wire up Redis Stream for order events
	maker.SetOrderEventCallback(func(event core.BotOrderEvent) {
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
		redis.PublishMMOrderEvent(context.Background(), mmEvent)
	})

	// Setup context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start bot
	if err := maker.Start(ctx); err != nil {
		log.Fatalf("Failed to start bot: %v", err)
	}

	// Update status in Redis
	statusKey := fmt.Sprintf("%s:%s", cfg.SimpleConfig.Symbol, botType)
	if err := redis.SetStatus(ctx, statusKey, "running"); err != nil {
		log.Printf("Failed to set status: %v", err)
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	log.Printf("Bot [%s] is running. Press Ctrl+C to stop.", botType)

	// Wait for first signal
	sig := <-sigCh
	log.Printf("Received signal: %v", sig)
	log.Println("Shutting down... (please wait for orders to be cancelled)")

	// Ignore further signals during shutdown
	signal.Stop(sigCh)

	// Update status
	if err := redis.SetStatus(context.Background(), statusKey, "stopped"); err != nil {
		log.Printf("Failed to set status: %v", err)
	}

	// Stop bot with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := maker.Stop(shutdownCtx); err != nil {
		log.Printf("Error during shutdown: %v", err)
	}

	log.Println("Bot stopped successfully")
}

// createSimpleMaker creates a SimpleMaker bot (2-sided)
func createSimpleMaker(
	cfg *config.Config,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
	exchangeName string,
	botID string,
) *core.BaseBot {
	simpleConfig := &cfg.SimpleConfig

	// Set defaults for embedded config
	if simpleConfig.SpreadMinBps == 0 {
		simpleConfig.SpreadMinBps = 40
	}
	if simpleConfig.SpreadMaxBps == 0 {
		simpleConfig.SpreadMaxBps = 100
	}
	if simpleConfig.NumLevels == 0 {
		simpleConfig.NumLevels = 5
	}
	if simpleConfig.TargetDepthNotional == 0 {
		simpleConfig.TargetDepthNotional = 10000
	}
	if simpleConfig.TickIntervalMs == 0 {
		simpleConfig.TickIntervalMs = 5000
	}
	if simpleConfig.LadderRegenBps == 0 {
		simpleConfig.LadderRegenBps = 50
	}
	if simpleConfig.LevelGapTicksMax == 0 {
		simpleConfig.LevelGapTicksMax = 20
	}
	if simpleConfig.DepthBps == 0 {
		simpleConfig.DepthBps = 200
	}
	if simpleConfig.FillCooldownMs == 0 {
		simpleConfig.FillCooldownMs = 5000
	}

	// Create simplified config - just embed MongoDB config + bot metadata
	makerCfg := &bot.SimpleMakerConfig{
		SimpleConfig:   simpleConfig, // Embed entire MongoDB config
		Exchange:       exchangeName,
		ExchangeID:     cfg.ExchangeID,
		BotID:          botID,
		BotType:        "simple-maker",
		TickIntervalMs: simpleConfig.TickIntervalMs,
		// Strategy-specific (not in MongoDB)
		DrawdownWarnPct:     simpleConfig.DrawdownLimitPct * 0.6,
		DrawdownReducePct:   simpleConfig.DrawdownLimitPct * 0.4,
		RecoveryHours:       48,
		MaxRecoverySizeMult: 0.3,
	}

	log.Printf("SimpleMaker config: spread=%.0fbps, levels=%d, depth=$%.0f, cooldown=%dms",
		simpleConfig.SpreadMinBps, simpleConfig.NumLevels, simpleConfig.TargetDepthNotional, simpleConfig.FillCooldownMs)

	return bot.NewSimpleMaker(makerCfg, exch, redis, mongo)
}

// createDepthFiller creates a DepthFiller bot (order book depth filler)
func createDepthFiller(
	cfg *config.Config,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
	exchangeName string,
	botID string,
) *core.BaseBot {
	simpleConfig := cfg.SimpleConfig

	// Read depth range from config, with defaults
	minDepthPct := simpleConfig.MinDepthPct
	if minDepthPct == 0 {
		minDepthPct = 5 // 5% from mid
	}
	maxDepthPct := simpleConfig.MaxDepthPct
	if maxDepthPct == 0 {
		maxDepthPct = 50 // 50% from mid
	}

	// Read min order size pct from config, with default
	minOrderSizePct := simpleConfig.MinOrderSizePct
	if minOrderSizePct == 0 {
		minOrderSizePct = 1 // 1% of balance per order
	}

	fillerCfg := &bot.DepthFillerConfig{
		Symbol:     simpleConfig.Symbol,
		BaseAsset:  simpleConfig.BaseAsset,
		QuoteAsset: simpleConfig.QuoteAsset,
		Exchange:   exchangeName,
		ExchangeID: cfg.ExchangeID,
		BotID:      botID,
		BotType:    "depth-filler",

		TickIntervalMs:      simpleConfig.TickIntervalMs,
		MinDepthPct:         minDepthPct,
		MaxDepthPct:         maxDepthPct,
		NumLevels:           simpleConfig.NumLevels,
		TargetDepthNotional: simpleConfig.TargetDepthNotional,
		TimeSleepMs:         200, // 200ms between orders
		RemoveThresholdPct:  minDepthPct,
		LadderRegenBps:      simpleConfig.LadderRegenBps,

		// Full balance mode
		UseFullBalance:  simpleConfig.UseFullBalance,
		MinOrderSizePct: minOrderSizePct,
	}

	// Set defaults
	if fillerCfg.TickIntervalMs == 0 {
		fillerCfg.TickIntervalMs = 5000
	}
	if fillerCfg.LadderRegenBps == 0 {
		fillerCfg.LadderRegenBps = 100 // Regen when mid moves 1%
	}
	if !fillerCfg.UseFullBalance && fillerCfg.TargetDepthNotional == 0 {
		fillerCfg.TargetDepthNotional = 100 // $100 per side default
	}
	if !fillerCfg.UseFullBalance && fillerCfg.NumLevels == 0 {
		fillerCfg.NumLevels = 10 // 10 levels per side default
	}

	if fillerCfg.UseFullBalance {
		log.Printf("DepthFiller config: depth=%.1f%%-%.1f%%, useFullBalance=true, minOrderSizePct=%.1f%%",
			fillerCfg.MinDepthPct, fillerCfg.MaxDepthPct, fillerCfg.MinOrderSizePct)
	} else {
		log.Printf("DepthFiller config: depth=%.1f%%-%.1f%%, levels=%d, notional=$%.0f",
			fillerCfg.MinDepthPct, fillerCfg.MaxDepthPct, fillerCfg.NumLevels, fillerCfg.TargetDepthNotional)
	}

	return bot.NewDepthFiller(fillerCfg, exch, redis, mongo)
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
