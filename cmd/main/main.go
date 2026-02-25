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
	"mm-platform-engine/internal/strategy"
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
		log.Fatal("BOT_TYPE is required (env or config.bot_type)")
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
	case "maker-bid", "bid":
		maker = createSimpleMaker(cfg, exch, redis, mongo, strategy.BotSideBid, exchangeName, botID)
		log.Println("Mode: MAKER-BID (BUY orders only)")

	case "maker-ask", "ask":
		maker = createSimpleMaker(cfg, exch, redis, mongo, strategy.BotSideAsk, exchangeName, botID)
		log.Println("Mode: MAKER-ASK (SELL orders only)")

	case "mm-engine", "engine", "mm":
		maker = createMMEngine(cfg, exch, redis, mongo, exchangeName, botID)
		log.Println("Mode: MM-ENGINE (TWO-SIDED market maker)")

	default:
		log.Fatalf("Invalid bot_type: %s (must be 'maker-bid', 'maker-ask', or 'mm-engine')", botType)
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

// createSimpleMaker creates a SimpleMaker bot (one-sided)
func createSimpleMaker(
	cfg *config.Config,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
	side strategy.BotSide,
	exchangeName string,
	botID string,
) *core.BaseBot {
	simpleConfig := cfg.SimpleConfig

	makerCfg := &bot.SimpleMakerConfig{
		Symbol:              simpleConfig.Symbol,
		BaseAsset:           simpleConfig.BaseAsset,
		QuoteAsset:          simpleConfig.QuoteAsset,
		Exchange:            exchangeName,
		ExchangeID:          cfg.UserExchangeKeyID,
		BotID:               botID,
		BotType:             string(side),
		TickIntervalMs:      simpleConfig.TickIntervalMs,
		BotSide:             side,
		SpreadBps:           simpleConfig.SpreadMinBps,
		NumLevels:           simpleConfig.NumLevels,
		TargetDepthNotional: simpleConfig.TargetDepthNotional,
		DepthBps:            simpleConfig.DepthBps,
		MinBalanceToTrade:   simpleConfig.MinBalanceToTrade,
		LadderRegenBps:      simpleConfig.LadderRegenBps,
		LevelGapTicksMax:    simpleConfig.LevelGapTicksMax,
		// Risk settings
		DrawdownLimitPct:    simpleConfig.DrawdownLimitPct,
		DrawdownWarnPct:     simpleConfig.DrawdownLimitPct * 0.6, // 60% of limit as warning
		DrawdownReducePct:   simpleConfig.DrawdownLimitPct * 0.4, // 40% of limit to start reducing
		RecoveryHours:       48,                                  // 48 hours target recovery
		MaxRecoverySizeMult: 0.3,                                 // 30% size at max drawdown
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

	log.Printf("SimpleMaker config: side=%s, spread=%.0fbps, levels=%d, depth=$%.0f",
		makerCfg.BotSide, makerCfg.SpreadBps, makerCfg.NumLevels, makerCfg.TargetDepthNotional)

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
		ExchangeID: cfg.UserExchangeKeyID,
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
