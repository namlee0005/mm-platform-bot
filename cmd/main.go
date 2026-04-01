package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mm-platform-engine/internal/bot"
	"mm-platform-engine/internal/config"
	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/engine"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/notify"
	"mm-platform-engine/internal/store"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	// Load config from MongoDB first (needed for log file naming)
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
		botType = "simple-maker"
	}

	exchangeName := strings.ToLower(cfg.ExchangeName)

	// Setup file logging: write to both stdout and log file
	// LOG_DIR env controls where logs go (default: ./logs, set "none" to disable)
	// File name format: <exchange>_<symbol>_<bottype>_<timestamp>.log
	logDir := os.Getenv("LOG_DIR")
	if logDir == "" {
		logDir = "logs"
	}
	if logDir == "none" {
		log.Println("Log output: stdout only (LOG_DIR=none)")
	} else {
		if err := os.MkdirAll(logDir, 0755); err != nil {
			log.Fatalf("Failed to create log directory: %v", err)
		}
		safeSymbol := strings.ReplaceAll(cfg.TradingConfig.Symbol, "/", "-")
		logFileName := filepath.Join(logDir, fmt.Sprintf("%s_%s_%s_%s.log",
			exchangeName, safeSymbol, botType,
			time.Now().Format("2006-01-02_15-04-05"),
		))
		logFile, err := os.OpenFile(logFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Fatalf("Failed to open log file: %v", err)
		}
		defer logFile.Close()
		log.SetOutput(io.MultiWriter(os.Stdout, logFile))
		log.Printf("Log file: %s", logFileName)
	}

	log.Println("========================================")
	log.Println("    Unified Market Making Bot")
	log.Println("========================================")

	log.Printf("Loaded config for %s on %s, bot_type=%s",
		cfg.TradingConfig.Symbol, cfg.ExchangeName, botType)

	// Create exchange client using CCXT
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

	// Setup context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Order event callback for Redis Stream
	orderEventCb := func(event core.BotOrderEvent) {
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
	}

	// Create engine based on bot type
	var eng *engine.Engine

	switch strings.ToLower(botType) {
	case "simple-maker":
		eng = createSimpleMakerEngine(cfg, exch, redis, mongo, exchangeName, botID, telegram)
		log.Println("Mode: SIMPLE-MAKER (event-driven)")
	case "spike-maker":
		eng = createSpikeMakerEngine(cfg, exch, redis, mongo, exchangeName, botID)
		log.Println("Mode: SPIKE-MAKER (event-driven)")
	case "spike-maker-v2":
		eng = createSpikeMakerV2Engine(cfg, exch, redis, mongo, exchangeName, botID)
		log.Println("Mode: SPIKE-MAKER-V2 (event-driven)")
	case "depth-filler":
		eng = createDepthFillerEngine(cfg, exch, redis, mongo, exchangeName, botID)
		log.Println("Mode: DEPTH-FILLER (event-driven)")
	default:
		log.Fatalf("Invalid bot_type: %s (must be 'simple-maker', 'spike-maker', 'spike-maker-v2', or 'depth-filler')", botType)
	}

	eng.SetFillNotifier(telegram)
	eng.SetOrderEventCallback(orderEventCb)

	if err := eng.Start(ctx); err != nil {
		log.Fatalf("Failed to start engine: %v", err)
	}

	startTime := time.Now()
	telegram.NotifyBotStarted(botType, map[string]interface{}{
		"symbol": cfg.SimpleConfig.Symbol,
	})

	statusKey := fmt.Sprintf("%s:%s", cfg.SimpleConfig.Symbol, botType)
	if err := redis.SetStatus(ctx, statusKey, "running"); err != nil {
		log.Printf("Failed to set status: %v", err)
	}

	// Wait for shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	log.Printf("Bot [%s] is running. Press Ctrl+C to stop.", botType)

	sig := <-sigCh
	log.Printf("Received signal: %v", sig)
	signal.Stop(sigCh)

	if err := redis.SetStatus(context.Background(), statusKey, "stopped"); err != nil {
		log.Printf("Failed to set status: %v", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := eng.Stop(shutdownCtx); err != nil {
		log.Printf("Error during shutdown: %v", err)
	}

	telegram.NotifyBotStopped("graceful shutdown", time.Since(startTime))
	log.Println("Bot stopped successfully")
}

// ===== Engine Factories =====

func createSimpleMakerEngine(
	cfg *config.Config,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
	exchangeName string,
	botID string,
	telegram *notify.TelegramNotifier,
) *engine.Engine {
	simpleConfig := &cfg.SimpleConfig

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

	makerCfg := &bot.SimpleMakerConfig{
		SimpleConfig:        simpleConfig,
		Exchange:            exchangeName,
		ExchangeID:          cfg.ExchangeID,
		BotID:               botID,
		BotType:             "simple-maker",
		TickIntervalMs:      simpleConfig.TickIntervalMs,
		DrawdownWarnPct:     simpleConfig.DrawdownLimitPct * 0.6,
		DrawdownReducePct:   simpleConfig.DrawdownLimitPct * 0.4,
		RecoveryHours:       48,
		MaxRecoverySizeMult: 0.3,
		OnModeChange: func(oldMode, newMode string, nav, peakNAV, drawdownPct float64) {
			reason := fmt.Sprintf("Drawdown: %.2f%%", drawdownPct*100)
			telegram.NotifyModeChange(oldMode, newMode, reason)
			if newMode == "WARNING" {
				telegram.NotifyDrawdownWarning(drawdownPct, nav, peakNAV)
			} else if newMode == "PAUSED" {
				telegram.NotifyDrawdownCritical(drawdownPct, nav, peakNAV)
			}
		},
		OnBalanceLow: func(available, required float64) {
			telegram.NotifyBalanceLow("Total Notional", available, required)
		},
	}

	return bot.NewSimpleMakerEngine(makerCfg, exch, redis, mongo)
}

func createSpikeMakerEngine(
	cfg *config.Config,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
	exchangeName string,
	botID string,
) *engine.Engine {
	simpleConfig := &cfg.SimpleConfig

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

	return bot.NewSpikeMakerEngine(spikeCfg, exch, redis, mongo)
}

func createSpikeMakerV2Engine(
	cfg *config.Config,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
	exchangeName string,
	botID string,
) *engine.Engine {
	simpleConfig := &cfg.SimpleConfig

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

	spikeCfg := &bot.SpikeMakerV2BotConfig{
		SimpleConfig:        simpleConfig,
		Exchange:            exchangeName,
		ExchangeID:          cfg.ExchangeID,
		BotID:               botID,
		BotType:             "spike-maker-v2",
		TickIntervalMs:      simpleConfig.TickIntervalMs,
		DrawdownWarnPct:     simpleConfig.DrawdownLimitPct * 0.6,
		DrawdownReducePct:   simpleConfig.DrawdownLimitPct * 0.4,
		MaxRecoverySizeMult: 0.3,
		ComplianceMode:      "capital",
	}

	return bot.NewSpikeMakerV2Engine(spikeCfg, exch, redis, mongo)
}

func createDepthFillerEngine(
	cfg *config.Config,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
	exchangeName string,
	botID string,
) *engine.Engine {
	simpleConfig := cfg.SimpleConfig

	minDepthPct := simpleConfig.MinDepthPct
	if minDepthPct == 0 {
		minDepthPct = 5
	}
	maxDepthPct := simpleConfig.MaxDepthPct
	if maxDepthPct == 0 {
		maxDepthPct = 50
	}
	minOrderSizePct := simpleConfig.MinOrderSizePct
	if minOrderSizePct == 0 {
		minOrderSizePct = 1
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
		TimeSleepMs:         200,
		RemoveThresholdPct:  minDepthPct,
		LadderRegenBps:      simpleConfig.LadderRegenBps,
		UseFullBalance:      simpleConfig.UseFullBalance,
		MinOrderSizePct:     minOrderSizePct,
	}

	if fillerCfg.TickIntervalMs == 0 {
		fillerCfg.TickIntervalMs = 5000
	}
	if fillerCfg.LadderRegenBps == 0 {
		fillerCfg.LadderRegenBps = 100
	}
	if !fillerCfg.UseFullBalance && fillerCfg.TargetDepthNotional == 0 {
		fillerCfg.TargetDepthNotional = 100
	}
	if !fillerCfg.UseFullBalance && fillerCfg.NumLevels == 0 {
		fillerCfg.NumLevels = 10
	}

	return bot.NewDepthFillerEngine(fillerCfg, exch, redis, mongo)
}
