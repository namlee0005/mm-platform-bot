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
	case "simple-maker", "maker", "maker-bid", "maker-ask", "bid", "ask":
		// All simple/one-sided types now use 2-sided simple-maker
		maker = createSimpleMaker(cfg, exch, redis, mongo, exchangeName, botID)
		log.Println("Mode: SIMPLE-MAKER (2-sided market maker)")

	case "mm-engine", "engine", "mm":
		maker = createMMEngine(cfg, exch, redis, mongo, exchangeName, botID)
		log.Println("Mode: MM-ENGINE (advanced 2-sided market maker)")

	case "depth-filler", "filler", "depth":
		maker = createDepthFiller(cfg, exch, redis, mongo, exchangeName, botID)
		log.Println("Mode: DEPTH-FILLER (order book depth filler)")

	default:
		log.Fatalf("Invalid bot_type: %s (must be 'simple-maker', 'mm-engine', or 'depth-filler')", botType)
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
	simpleConfig := cfg.SimpleConfig

	makerCfg := &bot.SimpleMakerConfig{
		Symbol:              simpleConfig.Symbol,
		BaseAsset:           simpleConfig.BaseAsset,
		QuoteAsset:          simpleConfig.QuoteAsset,
		Exchange:            exchangeName,
		ExchangeID:          cfg.ExchangeID,
		BotID:               botID,
		BotType:             "simple-maker",
		TickIntervalMs:      simpleConfig.TickIntervalMs,
		SpreadBps:           simpleConfig.SpreadMinBps,
		NumLevels:           simpleConfig.NumLevels,
		TargetDepthNotional: simpleConfig.TargetDepthNotional,
		DepthBps:            simpleConfig.DepthBps,
		MinBalanceToTrade:   simpleConfig.MinBalanceToTrade,
		LadderRegenBps:      simpleConfig.LadderRegenBps,
		LevelGapTicksMax:    simpleConfig.LevelGapTicksMax,
		// Risk settings
		DrawdownLimitPct:    simpleConfig.DrawdownLimitPct,
		DrawdownWarnPct:     simpleConfig.DrawdownLimitPct * 0.6,
		DrawdownReducePct:   simpleConfig.DrawdownLimitPct * 0.4,
		RecoveryHours:       48,
		MaxRecoverySizeMult: 0.3,
	}

	// Set defaults
	if makerCfg.SpreadBps == 0 {
		makerCfg.SpreadBps = 50
	}
	if makerCfg.NumLevels == 0 {
		makerCfg.NumLevels = 5
	}
	if makerCfg.TargetDepthNotional == 0 {
		makerCfg.TargetDepthNotional = 1000
	}
	if makerCfg.TickIntervalMs == 0 {
		makerCfg.TickIntervalMs = 5000
	}
	if makerCfg.LadderRegenBps == 0 {
		makerCfg.LadderRegenBps = 50
	}
	if makerCfg.LevelGapTicksMax == 0 {
		makerCfg.LevelGapTicksMax = 3
	}
	if makerCfg.DepthBps == 0 {
		makerCfg.DepthBps = 200
	}

	log.Printf("SimpleMaker config: spread=%.0fbps, levels=%d, depth=$%.0f",
		makerCfg.SpreadBps, makerCfg.NumLevels, makerCfg.TargetDepthNotional)

	return bot.NewSimpleMaker(makerCfg, exch, redis, mongo)
}

// createMMEngine creates an MMEngine bot (two-sided)
func createMMEngine(
	cfg *config.Config,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
	exchangeName string,
	botID string,
) *core.BaseBot {
	simpleConfig := cfg.SimpleConfig
	tradingConfig := cfg.TradingConfig

	engineCfg := &bot.MMEngineConfig{
		Symbol:     simpleConfig.Symbol,
		BaseAsset:  simpleConfig.BaseAsset,
		QuoteAsset: simpleConfig.QuoteAsset,
		Exchange:   exchangeName,
		ExchangeID: cfg.ExchangeID,
		BotID:      botID,
		BotType:    "mm-engine",

		TickIntervalMs:      simpleConfig.TickIntervalMs,
		BaseSpreadBps:       simpleConfig.SpreadMinBps,
		MinSpreadBps:        simpleConfig.SpreadMinBps,
		MaxSpreadBps:        simpleConfig.SpreadMaxBps,
		VolMultiplierCap:    3.0,
		NumLevels:           simpleConfig.NumLevels,
		OffsetsBps:          tradingConfig.OffsetsBps,
		SizeMult:            tradingConfig.SizeMult,
		QuotePerOrder:       tradingConfig.QuotePerOrder,
		TargetDepthNotional: simpleConfig.TargetDepthNotional,
		MinOrdersPerSide:    3,
		TargetRatio:         simpleConfig.TargetRatio,
		Deadzone:            tradingConfig.Deadzone,
		K:                   tradingConfig.K,
		MaxSkewBps:          tradingConfig.MaxSkewBps,
		MinOffsetBps:        tradingConfig.MinOffsetBps,
		DSkewMaxBpsPerTick:  tradingConfig.DSkewMaxBpsPerTick,
		SizeTiltCap:         0.3,
		ImbalanceThreshold:  simpleConfig.ImbalanceThreshold,
		RecoveryTarget:      0.05,

		DrawdownLimitPct:     simpleConfig.DrawdownLimitPct,
		DrawdownWarnPct:      simpleConfig.DrawdownLimitPct * 0.6,
		DrawdownAction:       "pause",
		MaxFillsPerMin:       simpleConfig.MaxFillsPerMin,
		CooldownAfterFillMs:  200,
		DefensiveCooldownSec: 60,

		PriceMovePct:       2.0,
		PriceMoveWindowSec: 10,
		VolSpikeMultiplier: 3.0,
		SweepDepthPct:      2.0,
		SweepMinDepth:      10.0,

		MicroOffsetTicks: 2,
		QtyJitterPct:     0.05,
		PriceJitterTicks: 3,

		DefensiveSpreadMult: 2.0,
		DefensiveSizeMult:   0.5,
		RecoverySpreadMult:  1.2,
		RecoverySizeMult:    1.0,
		PreferOneSided:      false,
	}

	// Set defaults
	if engineCfg.TickIntervalMs == 0 {
		engineCfg.TickIntervalMs = 5000
	}
	if engineCfg.NumLevels == 0 {
		engineCfg.NumLevels = 5
	}
	if engineCfg.TargetRatio == 0 {
		engineCfg.TargetRatio = 0.5
	}
	if engineCfg.DrawdownLimitPct == 0 {
		engineCfg.DrawdownLimitPct = 0.05
	}
	if engineCfg.MaxFillsPerMin == 0 {
		engineCfg.MaxFillsPerMin = 20
	}

	log.Printf("MMEngine config: levels=%d, target_ratio=%.2f, base_spread=%.0f bps",
		engineCfg.NumLevels, engineCfg.TargetRatio, engineCfg.BaseSpreadBps)

	return bot.NewMMEngine(engineCfg, exch, redis, mongo)
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

	fillerCfg := &bot.DepthFillerConfig{
		Symbol:     simpleConfig.Symbol,
		BaseAsset:  simpleConfig.BaseAsset,
		QuoteAsset: simpleConfig.QuoteAsset,
		Exchange:   exchangeName,
		ExchangeID: cfg.ExchangeID,
		BotID:      botID,
		BotType:    "depth-filler",

		TickIntervalMs:      simpleConfig.TickIntervalMs,
		MinDepthPct:         5,  // 5% from mid
		MaxDepthPct:         50, // 50% from mid
		NumLevels:           10, // 10 levels per side
		TargetDepthNotional: simpleConfig.TargetDepthNotional,
		TimeSleepMs:         200, // 200ms between orders
		RemoveThresholdPct:  5,   // Remove when within 5%
		LadderRegenBps:      100, // Regen when mid moves 1%
	}

	// Override NumLevels from config if available
	if simpleConfig.NumLevels > 0 {
		fillerCfg.NumLevels = simpleConfig.NumLevels
	}

	// Set defaults
	if fillerCfg.TickIntervalMs == 0 {
		fillerCfg.TickIntervalMs = 5000
	}
	if fillerCfg.TargetDepthNotional == 0 {
		fillerCfg.TargetDepthNotional = 100 // $100 per side default
	}

	log.Printf("DepthFiller config: depth=%.1f%%-%.1f%%, levels=%d, notional=$%.0f",
		fillerCfg.MinDepthPct, fillerCfg.MaxDepthPct, fillerCfg.NumLevels, fillerCfg.TargetDepthNotional)

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
