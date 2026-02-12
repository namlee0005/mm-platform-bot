package engine

import (
	"mm-platform-engine/internal/config"
)

// AdaptConfig converts legacy TradingConfig to new engine Config
func AdaptConfig(tradingCfg *config.TradingConfig) *Config {
	cfg := DefaultConfig()

	// Basic info
	cfg.Symbol = tradingCfg.Symbol
	cfg.BaseAsset = tradingCfg.BaseAsset
	cfg.QuoteAsset = tradingCfg.QuoteAsset

	// Spread config - derive from existing settings
	cfg.Spread.BaseSpreadBps = float64(tradingCfg.MinOffsetBps)
	cfg.Spread.MinSpreadBps = float64(tradingCfg.MinOffsetBps)
	cfg.Spread.MaxSpreadBps = float64(tradingCfg.MaxSkewBps) * 3 // 3x max skew as max spread

	// Depth config - from ladder settings
	cfg.Depth.NumLevels = len(tradingCfg.OffsetsBps)
	cfg.Depth.OffsetsBps = tradingCfg.OffsetsBps
	cfg.Depth.SizeMult = tradingCfg.SizeMult
	cfg.Depth.QuotePerOrder = tradingCfg.QuotePerOrder

	// Inventory config
	cfg.Inventory.TargetRatio = tradingCfg.TargetRatio
	cfg.Inventory.Deadzone = tradingCfg.Deadzone
	cfg.Inventory.K = tradingCfg.K
	cfg.Inventory.MaxSkewBps = tradingCfg.MaxSkewBps
	cfg.Inventory.MinOffsetBps = tradingCfg.MinOffsetBps
	cfg.Inventory.DSkewMaxBpsPerTick = tradingCfg.DSkewMaxBpsPerTick

	// Execution config - from replace thresholds
	// Use reprice threshold as tolerance (e.g., 30 bps reprice → 30 bps tolerance)
	cfg.Execution.PriceToleranceBps = tradingCfg.ReplaceThresholds.RepriceThresholdBps
	if cfg.Execution.PriceToleranceBps < 30 {
		cfg.Execution.PriceToleranceBps = 30 // minimum 30 bps tolerance for stable orders
	}
	cfg.Execution.InvDevThreshold = tradingCfg.ReplaceThresholds.InvDevThreshold
	cfg.Execution.MaxOrderAgeSec = tradingCfg.ReplaceThresholds.MaxOrderAgeSec
	cfg.Execution.RefreshBaseSec = tradingCfg.RefreshBaseSec
	cfg.Execution.RefreshJitterPct = tradingCfg.RefreshJitterPct

	// Risk config - from risk thresholds
	cfg.Risk.MaxFillsPerMin = tradingCfg.RiskThresholds.FillSpikePerMin

	// Mode overrides - from risk actions (only override if set, keep defaults otherwise)
	if tradingCfg.RiskActions.RiskSpreadMult > 0 {
		cfg.ModeOverrides.Defensive.SpreadMult = tradingCfg.RiskActions.RiskSpreadMult
	}
	if tradingCfg.RiskActions.RiskSizeMult > 0 {
		cfg.ModeOverrides.Defensive.SizeMult = tradingCfg.RiskActions.RiskSizeMult
	}
	if tradingCfg.RiskActions.RiskRefreshMult > 0 {
		cfg.ModeOverrides.Defensive.RefreshMult = tradingCfg.RiskActions.RiskRefreshMult
	}

	return cfg
}

// EngineConfigFromMongo returns an extended engine config that includes
// all the new MM engine parameters. This can be stored in MongoDB.
type EngineConfigMongo struct {
	// Basic
	Type       string `json:"type" bson:"type"`
	Symbol     string `json:"symbol" bson:"symbol"`
	BaseAsset  string `json:"base_asset" bson:"base_asset"`
	QuoteAsset string `json:"quote_asset" bson:"quote_asset"`

	// Engine Mode
	EngineMode string `json:"engine_mode" bson:"engine_mode"` // "legacy" or "mm_engine"

	// Spread
	Spread SpreadConfig `json:"spread" bson:"spread"`

	// Depth
	Depth DepthConfig `json:"depth" bson:"depth"`

	// Inventory
	Inventory InventoryConfig `json:"inventory" bson:"inventory"`

	// Risk
	Risk RiskConfig `json:"risk" bson:"risk"`

	// Shock detection
	Shock ShockConfig `json:"shock" bson:"shock"`

	// Execution
	Execution ExecutionConfig `json:"execution" bson:"execution"`

	// Anti-abuse
	AntiAbuse AntiAbuseConfig `json:"anti_abuse" bson:"anti_abuse"`

	// Reporting
	Reporting ReportingConfig `json:"reporting" bson:"reporting"`

	// Mode overrides
	ModeOverrides ModeOverrides `json:"mode_overrides" bson:"mode_overrides"`
}

// ToEngineConfig converts MongoDB config to engine.Config
func (m *EngineConfigMongo) ToEngineConfig() *Config {
	return &Config{
		Symbol:        m.Symbol,
		BaseAsset:     m.BaseAsset,
		QuoteAsset:    m.QuoteAsset,
		Spread:        m.Spread,
		Depth:         m.Depth,
		Inventory:     m.Inventory,
		Risk:          m.Risk,
		Shock:         m.Shock,
		Execution:     m.Execution,
		AntiAbuse:     m.AntiAbuse,
		Reporting:     m.Reporting,
		ModeOverrides: m.ModeOverrides,
	}
}

// DefaultEngineConfigMongo returns default config for MongoDB storage
func DefaultEngineConfigMongo(symbol, baseAsset, quoteAsset string) *EngineConfigMongo {
	def := DefaultConfig()
	return &EngineConfigMongo{
		Type:          "mm_engine",
		Symbol:        symbol,
		BaseAsset:     baseAsset,
		QuoteAsset:    quoteAsset,
		EngineMode:    "mm_engine",
		Spread:        def.Spread,
		Depth:         def.Depth,
		Inventory:     def.Inventory,
		Risk:          def.Risk,
		Shock:         def.Shock,
		Execution:     def.Execution,
		AntiAbuse:     def.AntiAbuse,
		Reporting:     def.Reporting,
		ModeOverrides: def.ModeOverrides,
	}
}
