package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"mm-platform-engine/internal/config"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/http"
	"mm-platform-engine/internal/metrics"
	"mm-platform-engine/internal/notify"
	"mm-platform-engine/internal/store"
	"mm-platform-engine/internal/types"
)

// Bot represents the main trading bot
type Bot struct {
	cfg               *config.Config
	userExchangeKeyID string // MongoDB ID for config reload
	exchange          exchange.Exchange
	redis             *store.RedisStore
	mongo             *store.MongoStore
	http              *http.Server

	// Observability
	promMetrics *metrics.Metrics
	telegram    *notify.TelegramNotifier
	startTime   time.Time

	// State
	mu         sync.RWMutex
	running    bool
	ctx        context.Context
	cancel     context.CancelFunc
	state      *types.EngineState
	metricsAgg *MetricsAggregator

	// UserStream reconnection management
	reconnectMu     sync.Mutex
	reconnecting    bool
	lastMessageTime time.Time
	streamConnected bool

	// Cached balance state from WebSocket
	balanceMu     sync.RWMutex
	cachedBalance map[string]*types.Balance // asset -> balance
}

// NewBot New creates a new Bot instance
func NewBot(cfg *config.Config) (*Bot, error) {
	// Create exchange client using CCXT adapter
	exchangeName := strings.ToLower(cfg.ExchangeName)
	sandbox := cfg.ExchangeBaseURL != "" && strings.Contains(cfg.ExchangeBaseURL, "testnet")

	exchangeClient, err := exchange.NewCCXTExchange(
		exchangeName,
		cfg.ExchangeAPIKey,
		cfg.ExchangeAPISecret,
		cfg.TradingConfig.Symbol,
		sandbox,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create exchange client: %w", err)
	}
	log.Printf("Using CCXT adapter for %s exchange", exchangeName)

	// Create Redis store
	redisStore, err := store.NewRedisStore(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	if err != nil {
		return nil, fmt.Errorf("failed to create redis store: %w", err)
	}

	// Create MongoDB store
	mongoStore, err := store.NewMongoStore(cfg.MongoURI, cfg.MongoDB)
	if err != nil {
		_ = redisStore.Close()
		return nil, fmt.Errorf("failed to create mongo store: %w", err)
	}

	// Create HTTP server
	httpServer := http.NewServer(cfg.HTTPPort, exchangeClient)

	// Initialize Prometheus metrics
	promMetrics := metrics.NewMetrics(
		"mmbot",
		exchangeName,
		cfg.TradingConfig.Symbol,
		cfg.UserExchangeKeyID,
	)

	// Initialize Telegram notifier
	telegramNotifier := notify.NewTelegramNotifier(notify.TelegramConfig{
		BotToken: cfg.TelegramBotToken,
		ChatID:   cfg.TelegramChatID,
		Exchange: exchangeName,
		Symbol:   cfg.TradingConfig.Symbol,
		BotID:    cfg.UserExchangeKeyID,
	})

	return &Bot{
		cfg:               cfg,
		userExchangeKeyID: cfg.UserExchangeKeyID,
		exchange:          exchangeClient,
		redis:             redisStore,
		mongo:             mongoStore,
		http:              httpServer,
		promMetrics:       promMetrics,
		telegram:          telegramNotifier,
		running:           false,
		cachedBalance:     make(map[string]*types.Balance),
	}, nil
}

// Start starts the bot
func (b *Bot) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return fmt.Errorf("bot is already running")
	}

	b.ctx, b.cancel = context.WithCancel(ctx)
	b.running = true
	b.startTime = time.Now()
	b.mu.Unlock()

	log.Println("Starting bot...")

	// Start exchange client
	if err := b.exchange.Start(b.ctx); err != nil {
		return fmt.Errorf("failed to start exchange: %w", err)
	}

	// Clear all existing orders on startup for a clean state
	symbol := b.cfg.TradingConfig.Symbol
	log.Printf("Clearing all existing orders for %s...", symbol)

	// Cancel all orders on the exchange
	if err := b.exchange.CancelAllOrders(b.ctx, symbol); err != nil {
		log.Printf("WARNING: Failed to cancel all orders on startup: %v", err)
		// Continue despite error - orders might already be empty
	} else {
		log.Printf("Successfully cancelled all orders on exchange")
	}

	// Clear all orders from Redis
	if err := b.redis.ClearAllOrders(b.ctx, strings.ToLower(b.cfg.ExchangeName), symbol); err != nil {
		log.Printf("WARNING: Failed to clear orders from Redis on startup: %v", err)
		// Continue despite error
	} else {
		log.Printf("Successfully cleared all orders from Redis")
	}

	// Start HTTP server
	go func() {
		if err := b.http.Start(b.ctx); err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	// Start user stream
	if err := b.startUserStream(b.ctx); err != nil {
		return fmt.Errorf("failed to start user stream: %w", err)
	}

	// Initialize metrics aggregator and load fill history
	windowMs := int64(5 * 60 * 1000) // 5 minutes window
	b.metricsAgg = NewMetricsAggregator(windowMs)
	if err := b.loadFillHistory(b.ctx, windowMs); err != nil {
		log.Printf("WARNING: Failed to load fill history: %v", err)
		// Continue despite error - metrics will start fresh
	}

	// Start main trading loop in goroutine
	go b.mainLoop()

	// Update status in Redis
	if err := b.redis.SetStatus(b.ctx, b.cfg.TradingConfig.Symbol, "running"); err != nil {
		log.Printf("Failed to set status in redis: %v", err)
	}

	log.Println("Bot started successfully")

	// Send Telegram notification
	b.telegram.NotifyBotStarted("simple-maker", map[string]interface{}{
		"symbol": b.cfg.TradingConfig.Symbol,
	})

	return nil
}

// Stop gracefully stops the bot
func (b *Bot) Stop(ctx context.Context) error {
	b.mu.Lock()
	if !b.running {
		b.mu.Unlock()
		return fmt.Errorf("bot is not running")
	}
	b.mu.Unlock()

	log.Println("Stopping bot...")

	// Update status in Redis BEFORE shutdown (so Redis is still available)
	if err := b.redis.SetStatus(ctx, b.cfg.TradingConfig.Symbol, "stopped"); err != nil {
		log.Printf("Failed to set status in redis: %v", err)
	}

	// Cancel context to stop all goroutines
	if b.cancel != nil {
		b.cancel()
	}

	// Shutdown gracefully (this will close Redis, Mongo, etc.)
	if err := b.shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown error: %w", err)
	}

	b.mu.Lock()
	b.running = false
	runtime := time.Since(b.startTime)
	b.mu.Unlock()

	// Send Telegram notification
	b.telegram.NotifyBotStopped("graceful shutdown", runtime)

	log.Println("Bot stopped successfully")
	return nil
}

// IsRunning returns whether the bot is running
func (b *Bot) IsRunning() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.running
}

// loadFillHistory loads fill history from MongoDB to warm up metrics
func (b *Bot) loadFillHistory(ctx context.Context, windowMs int64) error {
	symbol := b.cfg.TradingConfig.Symbol
	sinceTime := time.Now().Add(-time.Duration(windowMs) * time.Millisecond)

	log.Printf("Loading fill history for %s since %s...", symbol, sinceTime.Format("15:04:05"))

	fills, err := b.mongo.GetFillsInWindow(ctx, symbol, sinceTime)
	if err != nil {
		return fmt.Errorf("failed to get fills from MongoDB: %w", err)
	}

	// Add fills to metrics aggregator
	for _, fill := range fills {
		side := types.OrderSideBuy
		if fill.Side == "SELL" {
			side = types.OrderSideSell
		}
		b.metricsAgg.RecordFill(side, fill.Price, fill.Quantity, fill.Timestamp.UnixMilli())
	}

	log.Printf("Loaded %d fills from history into metrics aggregator", len(fills))
	return nil
}

// mainLoop runs the main trading loop
func (b *Bot) mainLoop() {
	log.Println("Starting main trading loop...")

	// Tick interval: 3 seconds (can be made configurable later)
	tickInterval := 5 * time.Second

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	// Run once immediately on startup
	log.Printf("Running initial tick...")
	if err := b.run(b.ctx); err != nil {
		log.Printf("ERROR: Initial tick failed: %v", err)
	}

	// Main loop
	for {
		select {
		case <-b.ctx.Done():
			log.Println("Main loop stopped by context cancellation")
			return

		case <-ticker.C:
			// Check for config updates before each tick
			b.checkAndReloadConfig()

			log.Printf("Running tick at %s...", time.Now().Format("15:04:05"))
			// Run the trading logic
			if err := b.run(b.ctx); err != nil {
				log.Printf("ERROR: Trading tick failed: %v", err)
				// Continue running despite errors
				// Add exponential backoff or circuit breaker here if needed
			}
		}
	}
}

// checkAndReloadConfig checks if config has been updated in MongoDB and reloads it
func (b *Bot) checkAndReloadConfig() {
	if b.userExchangeKeyID == "" {
		return
	}

	configUpdate, err := b.mongo.CheckConfigUpdate(b.ctx, b.userExchangeKeyID)
	if err != nil {
		log.Printf("WARNING: Failed to check config update: %v", err)
		return
	}

	if !configUpdate.IsUpdated || configUpdate.SimpleConfig == nil {
		return
	}

	log.Printf("Config update detected, reloading simple_config...")

	// Convert SimpleConfigUpdate to SimpleConfig for ToTradingConfig
	simpleConfig := config.SimpleConfig{
		Symbol:              configUpdate.SimpleConfig.Symbol,
		BaseAsset:           configUpdate.SimpleConfig.BaseAsset,
		QuoteAsset:          configUpdate.SimpleConfig.QuoteAsset,
		SpreadMinBps:        configUpdate.SimpleConfig.SpreadMinBps,
		SpreadMaxBps:        configUpdate.SimpleConfig.SpreadMaxBps,
		NumLevels:           configUpdate.SimpleConfig.NumLevels,
		TargetDepthNotional: configUpdate.SimpleConfig.TargetDepthNotional,
		InitBase:            configUpdate.SimpleConfig.InitBase,
		InitQuote:           configUpdate.SimpleConfig.InitQuote,
		PyramidFactor:       configUpdate.SimpleConfig.PyramidFactor,
		DrawdownLimitPct:    configUpdate.SimpleConfig.DrawdownLimitPct,
		MaxFillsPerMin:      configUpdate.SimpleConfig.MaxFillsPerMin,
		SkewK:               configUpdate.SimpleConfig.SkewK,
		MaxSkewBps:          configUpdate.SimpleConfig.MaxSkewBps,
		ImbalanceThreshold:  configUpdate.SimpleConfig.ImbalanceThreshold,
		TickIntervalMs:      configUpdate.SimpleConfig.TickIntervalMs,
	}

	// Convert to TradingConfig
	newTradingConfig := simpleConfig.ToTradingConfig()

	// Update trading config
	b.mu.Lock()
	b.cfg.TradingConfig = newTradingConfig
	b.mu.Unlock()

	// Cancel all orders on exchange and clear Redis to force new orders with new config
	symbol := b.cfg.TradingConfig.Symbol
	log.Printf("Cancelling all orders for %s due to config reload...", symbol)

	if err := b.exchange.CancelAllOrders(b.ctx, symbol); err != nil {
		log.Printf("WARNING: Failed to cancel orders on config reload: %v", err)
	}

	if err := b.redis.ClearAllOrders(b.ctx, strings.ToLower(b.cfg.ExchangeName), symbol); err != nil {
		log.Printf("WARNING: Failed to clear Redis orders on config reload: %v", err)
	}

	// Reset engine state to force immediate order placement
	b.state = &types.EngineState{}

	log.Printf("Config reloaded successfully: QuotePerOrder=%.2f, K=%.2f, Levels=%d",
		newTradingConfig.QuotePerOrder,
		newTradingConfig.K,
		len(newTradingConfig.OffsetsBps))
}
