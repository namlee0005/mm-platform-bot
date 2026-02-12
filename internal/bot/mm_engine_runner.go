package bot

import (
	"context"
	"fmt"
	"log"
	"time"

	"mm-platform-engine/internal/engine"
	"mm-platform-engine/internal/store"
	"mm-platform-engine/internal/types"
)

// MMEngineRunner wraps the MM engine to work with the existing bot infrastructure
type MMEngineRunner struct {
	engine *engine.Engine
	cfg    *engine.Config
	bot    *Bot // reference to parent bot for WebSocket events
	redis  *store.RedisStore
	botID  string // unique bot instance ID
}

// NewMMEngineRunner creates a new MM engine runner
func NewMMEngineRunner(
	cfg *engine.Config,
	bot *Bot,
	redis *store.RedisStore,
	mongo *store.MongoStore,
	botID string,
) *MMEngineRunner {
	// Create the engine with exchange, redis, and mongo
	eng := engine.NewEngine(cfg, bot.exchange, redis, mongo)

	runner := &MMEngineRunner{
		engine: eng,
		cfg:    cfg,
		bot:    bot,
		redis:  redis,
		botID:  botID,
	}

	// Wire up Redis Stream for order events
	// NestJS gateway subscribes to mm:stream:{symbol}
	if redis != nil {
		eng.SetOrderEventCallback(runner.publishOrderEvent)
		log.Printf("[MM_ENGINE] Order events will be published to Redis stream mm:stream:%s (botID=%s)", cfg.Symbol, botID)
	}

	return runner
}

// publishOrderEvent publishes order event to Redis Stream
func (r *MMEngineRunner) publishOrderEvent(event engine.OrderEvent) {
	if r.redis == nil {
		return
	}

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
		BotID:     r.botID,
	}

	// Use background context since bot.ctx might not be available
	ctx := context.Background()
	if r.bot != nil && r.bot.ctx != nil {
		ctx = r.bot.ctx
	}

	if err := r.redis.PublishMMOrderEvent(ctx, mmEvent); err != nil {
		log.Printf("[REDIS] Failed to publish %s event: %v", event.Type, err)
	} else {
		log.Printf("[REDIS] Published %s %s L%d %s @ %.8f (id=%s, botID=%s)",
			event.Type, event.Side, event.Level, event.Symbol, event.Price, event.OrderID, r.botID)
	}
}

// Start starts the MM engine
func (r *MMEngineRunner) Start(ctx context.Context) error {
	log.Println("[MM_ENGINE] Starting MM Engine...")
	return r.engine.Start(ctx)
}

// Stop stops the MM engine
func (r *MMEngineRunner) Stop(ctx context.Context) error {
	log.Println("[MM_ENGINE] Stopping MM Engine...")
	return r.engine.Stop(ctx)
}

// GetMode returns the current engine mode
func (r *MMEngineRunner) GetMode() engine.Mode {
	return r.engine.GetMode()
}

// GetMetrics returns recent metrics
func (r *MMEngineRunner) GetMetrics(n int) []engine.EngineMetrics {
	return r.engine.GetMetrics(n)
}

// GetTradeLog returns sanitized trade log
func (r *MMEngineRunner) GetTradeLog(limit int) []engine.TradeLogEntry {
	return r.engine.GetTradeLog(limit)
}

// ForceMode allows manual mode override
func (r *MMEngineRunner) ForceMode(mode engine.Mode, reason string) {
	r.engine.ForceMode(mode, reason)
}

// OnAccountUpdate handles account updates from WebSocket
// This is called by the bot's user stream handler
func (r *MMEngineRunner) OnAccountUpdate(event *types.AccountEvent) {
	// The engine handles this internally via its own subscribeUserStream
	// This is provided for compatibility if needed
}

// OnOrderUpdate handles order updates from WebSocket
func (r *MMEngineRunner) OnOrderUpdate(event *types.OrderEvent) {
	// The engine handles this internally
}

// OnFill handles fill events from WebSocket
func (r *MMEngineRunner) OnFill(event *types.FillEvent) {
	// The engine handles this internally
}

// =========================================================================
// Integration with existing Bot
// =========================================================================

// UseMMEngine switches the bot to use the new MM engine instead of legacy logic
// Call this in Bot.Start() instead of starting the legacy mainLoop
func (b *Bot) UseMMEngine(ctx context.Context) error {
	// Convert existing config to engine config
	engineCfg := engine.AdaptConfig(&b.cfg.TradingConfig)

	// Use UserExchangeKeyID as botID
	botID := b.cfg.UserExchangeKeyID

	log.Printf("[BOT] Using MM Engine with config: symbol=%s, levels=%d, target_ratio=%.2f, botID=%s",
		engineCfg.Symbol, engineCfg.Depth.NumLevels, engineCfg.Inventory.TargetRatio, botID)

	// Create MM engine runner
	runner := NewMMEngineRunner(engineCfg, b, b.redis, b.mongo, botID)

	// Start the engine (this replaces the legacy mainLoop)
	if err := runner.Start(ctx); err != nil {
		return fmt.Errorf("failed to start MM engine: %w", err)
	}

	// Store runner reference if needed for admin controls
	// b.mmEngine = runner

	return nil
}

// PrintEngineStatus prints the current engine status for monitoring
func (r *MMEngineRunner) PrintEngineStatus() {
	mode := r.GetMode()
	metrics := r.GetMetrics(1)

	log.Println("========== MM ENGINE STATUS ==========")
	log.Printf("Mode: %s", mode)

	if len(metrics) > 0 {
		m := metrics[0]
		log.Printf("Mid: %.6f", m.Mid)
		log.Printf("Spread: %.2f bps", m.AvgSpreadBps)
		log.Printf("Depth: $%.2f", m.DepthNotional)
		log.Printf("Inventory: %.2f%% (target: 50%%)", m.InvRatio*100)
		log.Printf("Drawdown 24h: %.2f%%", m.Drawdown24h*100)
		log.Printf("Fills/min: %.1f", m.FillsPerMin)
		log.Printf("Uptime: %.1f%%", m.QuoteUptime*100)
		log.Printf("NAV: $%.2f", m.NAV)
	}

	log.Println("=======================================")
}

// MonitorLoop starts a background goroutine that periodically prints status
func (r *MMEngineRunner) MonitorLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.PrintEngineStatus()
		}
	}
}
