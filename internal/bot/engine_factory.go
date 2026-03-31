package bot

import (
	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/engine"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/modules"
	"mm-platform-engine/internal/store"
	"mm-platform-engine/internal/strategy"
)

// NewSimpleMakerEngine creates a SimpleMaker using the event-driven Engine.
func NewSimpleMakerEngine(
	cfg *SimpleMakerConfig,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
) *engine.Engine {
	// Strategy
	strategyCfg := &strategy.SimpleLadderConfig{
		SimpleConfig:        cfg.SimpleConfig,
		PriceJitterPct:      cfg.PriceJitterPct,
		SizeJitterPct:       cfg.SizeJitterPct,
		DrawdownWarnPct:     cfg.DrawdownWarnPct,
		DrawdownReducePct:   cfg.DrawdownReducePct,
		RecoveryHours:       cfg.RecoveryHours,
		MaxRecoverySizeMult: cfg.MaxRecoverySizeMult,
		OnModeChange:        cfg.OnModeChange,
		OnBalanceLow:        cfg.OnBalanceLow,
	}
	strat := strategy.NewSimpleLadderStrategy(strategyCfg)

	return createEngine(strat, cfg.SimpleConfig.Symbol, cfg.SimpleConfig.BaseAsset, cfg.SimpleConfig.QuoteAsset,
		cfg.Exchange, cfg.ExchangeID, cfg.BotID, cfg.BotType, cfg.TickIntervalMs,
		exch, redis, mongo)
}

// NewSpikeMakerEngine creates a SpikeMaker using the event-driven Engine.
func NewSpikeMakerEngine(
	cfg *SpikeMakerBotConfig,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
) *engine.Engine {
	strategyCfg := &strategy.SpikeMakerConfig{
		SimpleConfig:        cfg.SimpleConfig,
		DrawdownWarnPct:     cfg.DrawdownWarnPct,
		DrawdownReducePct:   cfg.DrawdownReducePct,
		MaxRecoverySizeMult: cfg.MaxRecoverySizeMult,
	}
	strat := strategy.NewSpikeMakerStrategy(strategyCfg)

	return createEngine(strat, cfg.SimpleConfig.Symbol, cfg.SimpleConfig.BaseAsset, cfg.SimpleConfig.QuoteAsset,
		cfg.Exchange, cfg.ExchangeID, cfg.BotID, cfg.BotType, cfg.TickIntervalMs,
		exch, redis, mongo)
}

// NewSpikeMakerV2Engine creates a SpikeMakerV2 using timer-only mode (5s interval).
func NewSpikeMakerV2Engine(
	cfg *SpikeMakerV2BotConfig,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
) *engine.Engine {
	strategyCfg := &strategy.SpikeMakerV2Config{
		SimpleConfig:        cfg.SimpleConfig,
		DrawdownWarnPct:     cfg.DrawdownWarnPct,
		DrawdownReducePct:   cfg.DrawdownReducePct,
		MaxRecoverySizeMult: cfg.MaxRecoverySizeMult,
		ComplianceMode:      cfg.ComplianceMode,
	}
	strat := strategy.NewSpikeMakerV2Strategy(strategyCfg)

	eng := createEngine(strat, cfg.SimpleConfig.Symbol, cfg.SimpleConfig.BaseAsset, cfg.SimpleConfig.QuoteAsset,
		cfg.Exchange, cfg.ExchangeID, cfg.BotID, cfg.BotType, cfg.TickIntervalMs,
		exch, redis, mongo)

	// V2: timer-only mode — Tick() runs every 5s instead of on WS events
	eng.SetTimerOnlyMode(5000)

	return eng
}

// NewDepthFillerEngine creates a DepthFiller using the event-driven Engine.
func NewDepthFillerEngine(
	cfg *DepthFillerConfig,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
) *engine.Engine {
	strategyCfg := &strategy.DepthFillerConfig{
		MinDepthPct:         cfg.MinDepthPct,
		MaxDepthPct:         cfg.MaxDepthPct,
		NumLevels:           cfg.NumLevels,
		TargetDepthNotional: cfg.TargetDepthNotional,
		TimeSleepMs:         cfg.TimeSleepMs,
		RemoveThresholdPct:  cfg.RemoveThresholdPct,
		LadderRegenBps:      cfg.LadderRegenBps,
		UseFullBalance:      cfg.UseFullBalance,
		MinOrderSizePct:     cfg.MinOrderSizePct,
	}
	strat := strategy.NewDepthFillerStrategy(strategyCfg)

	return createEngine(strat, cfg.Symbol, cfg.BaseAsset, cfg.QuoteAsset,
		cfg.Exchange, cfg.ExchangeID, cfg.BotID, cfg.BotType, cfg.TickIntervalMs,
		exch, redis, mongo)
}

// createEngine is the shared factory that wires Engine + Executor + Strategy.
func createEngine(
	strat core.Strategy,
	symbol, baseAsset, quoteAsset string,
	exchangeName, exchangeID, botID, botType string,
	tickIntervalMs int,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
) *engine.Engine {
	engineCfg := &engine.EngineConfig{
		Symbol:     symbol,
		BaseAsset:  baseAsset,
		QuoteAsset: quoteAsset,
		Exchange:   exchangeName,
		ExchangeID: exchangeID,
		BotID:      botID,
		BotType:    botType,

		// Coalescing
		CoalesceWindowMs:    50,
		MaxCoalesceMs:       200,
		RepriceThresholdBps: 5,
		BackstopIntervalMs:  30000,
		BusCapacity:         256,

		// Execution
		RateLimitOrdersPerSec: 10,
		BatchSize:             20,
		PriceToleranceBps:     2,
		QtyTolerancePct:       0.05,
		MinOrderLifetimeMs:    1000,

		// Config check
		ConfigCheckIntervalMs:  10000,
		FallbackPollIntervalMs: 3000,
	}
	engineCfg.ApplyDefaults()

	// Execution engine (reuse existing modules)
	execCfg := &modules.ExecutionConfig{
		RateLimitOrdersPerSec: engineCfg.RateLimitOrdersPerSec,
		BatchSize:             engineCfg.BatchSize,
		PriceToleranceBps:     engineCfg.PriceToleranceBps,
		QtyTolerancePct:       engineCfg.QtyTolerancePct,
		MinOrderLifetimeMs:    engineCfg.MinOrderLifetimeMs,
	}
	execEngine := modules.NewExecutionEngine(execCfg, nil, exch, symbol)
	diffEngine := modules.NewOrderDiffEngine(execCfg)
	executor := engine.NewExecutor(exch, symbol, diffEngine, execEngine)

	return engine.NewEngine(engineCfg, strat, exch, executor, redis, mongo)
}
