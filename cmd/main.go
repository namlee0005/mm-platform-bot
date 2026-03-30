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
	"mm-platform-engine/internal/notify"
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

	// Create exchange client using CCXT
	exchangeName := strings.ToLower(cfg.ExchangeName)
	sandbox := cfg.ExchangeBaseURL != "" && strings.Contains(cfg.ExchangeBaseURL, "testnet")

	exch, err := exchange.NewCCXTExchange(
		exchangeName,
		cfg.ExchangeAPIKey,
		cfg.ExchangeAPISecret,
		cfg.TradingConfig.Symbol,
		sandbox,
	)
	if err != nil {
		log.Fatalf("Failed to create exchange client: %v", err)
	}
	log.Printf("Using CCXT adapter for %s exchange", exchangeName)

	// Create Redis store
	redis, err := store.NewRedisStore(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB, cfg.Env)
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

	// Create Telegram notifier
	telegram := notify.NewTelegramNotifier(notify.TelegramConfig{
		BotToken: cfg.TelegramBotToken,
		ChatID:   cfg.TelegramChatID,
		Exchange: exchangeName,
		Symbol:   cfg.TradingConfig.Symbol,
		BotID:    botID,
	})

	// Create bot based on type
	var maker *core.BaseBot

	switch strings.ToLower(botType) {
	case "simple-maker":
		// All simple/one-sided types now use 2-sided simple-maker
		maker = createSimpleMaker(cfg, exch, redis, mongo, exchangeName, botID, telegram)
		log.Println("Mode: SIMPLE-MAKER (2-sided market maker)")

	case "spike-maker":
		maker = createSpikeMaker(cfg, exch, redis, mongo, exchangeName, botID)
		log.Println("Mode: SPIKE-MAKER (spike-adaptive market maker)")

	case "depth-filler":
		maker = createDepthFiller(cfg, exch, redis, mongo, exchangeName, botID)
		log.Println("Mode: DEPTH-FILLER (order book depth filler)")

	default:
		log.Fatalf("Invalid bot_type: %s (must be 'simple-maker', 'spike-maker', or 'depth-filler')", botType)
	}

	// Wire up fill notifications (Telegram) — co-located with SaveFill in BaseBot
	maker.SetFillNotifier(telegram)

	// Wire up Redis Stream for order events
	maker.SetOrderEventCallback(func(event core.BotOrderEvent) {
		redis.PublishMMOrderEvent(context.Background(), &store.MMOrderEvent{
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
		})
	})

	// Setup context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start bot
	if err := maker.Start(ctx); err != nil {
		log.Fatalf("Failed to start bot: %v", err)
	}

	// Send Telegram notification for bot start
	startTime := time.Now()
	telegram.NotifyBotStarted(botType, map[string]interface{}{
		"symbol": cfg.SimpleConfig.Symbol,
	})

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

	// Send Telegram notification for bot stop
	runtime := time.Since(startTime)
	telegram.NotifyBotStopped("graceful shutdown", runtime)

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
	telegram *notify.TelegramNotifier,
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
		// Telegram notifications for mode changes
		OnModeChange: func(oldMode, newMode string, nav, peakNAV, drawdownPct float64) {
			// Determine reason for mode change
			reason := fmt.Sprintf("Drawdown: %.2f%%", drawdownPct*100)

			// Send mode change notification
			telegram.NotifyModeChange(oldMode, newMode, reason)

			// Send specific drawdown alerts based on severity
			if newMode == "WARNING" {
				telegram.NotifyDrawdownWarning(drawdownPct, nav, peakNAV)
			} else if newMode == "PAUSED" {
				telegram.NotifyDrawdownCritical(drawdownPct, nav, peakNAV)
			}
		},
		// Telegram notification for low balance
		OnBalanceLow: func(available, required float64) {
			telegram.NotifyBalanceLow("Total Notional", available, required)
		},
	}

	log.Printf("SimpleMaker config: spread=%.0fbps, levels=%d, depth=$%.0f, cooldown=%dms",
		simpleConfig.SpreadMinBps, simpleConfig.NumLevels, simpleConfig.TargetDepthNotional, simpleConfig.FillCooldownMs)

	return bot.NewSimpleMaker(makerCfg, exch, redis, mongo)
}

// createSpikeMaker creates a SpikeMaker bot (spike-adaptive 2-sided market maker)
func createSpikeMaker(
	cfg *config.Config,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
	exchangeName string,
	botID string,
) *core.BaseBot {
	simpleConfig := &cfg.SimpleConfig

	// Set defaults
	if simpleConfig.NumLevels == 0 {
		simpleConfig.NumLevels = 10
	}
	if simpleConfig.TargetDepthNotional == 0 {
		simpleConfig.TargetDepthNotional = 5800
	}
	if simpleConfig.TickIntervalMs == 0 {
		simpleConfig.TickIntervalMs = 5000
	}
	if simpleConfig.DepthBps == 0 {
		simpleConfig.DepthBps = 200
	}

	spikeCfg := &bot.SpikeMakerBotConfig{
		SimpleConfig:        simpleConfig,
		Exchange:            exchangeName,
		ExchangeID:          cfg.ExchangeID,
		BotID:               botID,
		BotType:             "spike-maker",
		TickIntervalMs:      simpleConfig.TickIntervalMs,
		DrawdownWarnPct:     simpleConfig.DrawdownLimitPct * 0.6,
		DrawdownReducePct:   simpleConfig.DrawdownLimitPct * 0.4,
		MaxRecoverySizeMult: 0.3,
	}

	log.Printf("SpikeMaker config: levels=%d, depth=$%.0f, depthBps=%.0f, tick=%dms",
		simpleConfig.NumLevels, simpleConfig.TargetDepthNotional, simpleConfig.DepthBps, simpleConfig.TickIntervalMs)

	return bot.NewSpikeMaker(spikeCfg, exch, redis, mongo)
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
