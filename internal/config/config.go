package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type TradingConfig struct {
	Symbol     string `json:"symbol"`
	BaseAsset  string `json:"base_asset"`
	QuoteAsset string `json:"quote_asset"`

	// Inventory target
	TargetRatio float64 `json:"target_ratio"` // Target inventory ratio (0.5 = 50/50)

	// Ladder configuration
	OffsetsBps    []int     `json:"offsets_bps"`     // Offsets from mid in basis points (must be increasing)
	SizeMult      []float64 `json:"size_mult"`       // Size multipliers for each level (must be non-increasing)
	QuotePerOrder float64   `json:"quote_per_order"` // Base quote amount per order

	// Skew parameters
	Deadzone           float64 `json:"deadzone"`                // Inventory deviation deadzone (no skew if within)
	K                  float64 `json:"k"`                       // Skew sensitivity factor
	MaxSkewBps         int     `json:"max_skew_bps"`            // Maximum skew in basis points
	MinOffsetBps       int     `json:"min_offset_bps"`          // Minimum offset from mid in basis points
	DSkewMaxBpsPerTick int     `json:"d_skew_max_bps_per_tick"` // Maximum skew change per tick (rate limit)

	// Replace thresholds
	ReplaceThresholds ReplaceThresholds `json:"replace_thresholds"`

	// Risk thresholds
	RiskThresholds RiskThresholds `json:"risk_thresholds"`

	// Risk actions
	RiskActions RiskActions `json:"risk_actions"`

	// Refresh configuration
	RefreshBaseSec   int     `json:"refresh_base_sec"`   // Base refresh interval in seconds
	RefreshJitterPct float64 `json:"refresh_jitter_pct"` // Refresh jitter percentage (e.g., 0.15 = ±15%)
}

type Config struct {
	// Exchange settings
	ExchangeAPIKey    string
	ExchangeAPISecret string
	ExchangeBaseURL   string

	// Trading settings
	TradingConfig TradingConfig

	// Redis settings
	RedisAddr     string
	RedisPassword string
	RedisDB       int

	// MongoDB settings
	MongoURI        string
	MongoDB         string
	MongoCollection string

	// HTTP server settings
	HTTPPort int

	// Operational settings
	LogLevel string
}

// Load reads configuration from environment variables
// Automatically loads from .env file if it exists
func Load(path string) (*Config, error) {
	// Load .env file if it exists (optional)
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using system environment variables")
	} else {
		log.Println("Loaded configuration from .env file")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	var tradingConfig TradingConfig
	if err := json.Unmarshal(data, &tradingConfig); err != nil {
		return nil, fmt.Errorf("failed to parse config JSON: %w", err)
	}

	if err := tradingConfig.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	cfg := &Config{
		// Exchange
		ExchangeAPIKey:    getEnv("EXCHANGE_API_KEY", ""),
		ExchangeAPISecret: getEnv("EXCHANGE_API_SECRET", ""),
		ExchangeBaseURL:   getEnv("EXCHANGE_BASE_URL", "https://api.mexc.com"),

		// Trading
		TradingConfig: tradingConfig,

		// Redis
		RedisAddr:     getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword: getEnv("REDIS_PASSWORD", ""),
		RedisDB:       getEnvInt("REDIS_DB", 0),

		// MongoDB
		MongoURI:        getEnv("MONGO_URI", "mongodb://localhost:27017"),
		MongoDB:         getEnv("MONGO_DB", "bot_engine"),
		MongoCollection: getEnv("MONGO_COLLECTION", "fills"),

		// HTTP
		HTTPPort: getEnvInt("HTTP_PORT", 8080),

		// Operational
		LogLevel: getEnv("LOG_LEVEL", "info"),
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return cfg, nil
}

// Validate checks if required configuration values are set
func (c *Config) Validate() error {
	if c.ExchangeAPIKey == "" {
		return fmt.Errorf("EXCHANGE_API_KEY is required")
	}
	if c.ExchangeAPISecret == "" {
		return fmt.Errorf("EXCHANGE_API_SECRET is required")
	}
	return nil
}

// Validate checks if the configuration is valid
func (c *TradingConfig) Validate() error {
	if len(c.OffsetsBps) == 0 {
		return fmt.Errorf("offsets_bps must not be empty")
	}

	if len(c.SizeMult) != len(c.OffsetsBps) {
		return fmt.Errorf("size_mult length must match offsets_bps length")
	}

	// Check offsets are increasing
	for i := 1; i < len(c.OffsetsBps); i++ {
		if c.OffsetsBps[i] <= c.OffsetsBps[i-1] {
			return fmt.Errorf("offsets_bps must be strictly increasing")
		}
	}

	// Check size multipliers are non-increasing
	for i := 1; i < len(c.SizeMult); i++ {
		if c.SizeMult[i] > c.SizeMult[i-1] {
			return fmt.Errorf("size_mult must be non-increasing")
		}
	}

	if c.TargetRatio < 0 || c.TargetRatio > 1 {
		return fmt.Errorf("target_ratio must be between 0 and 1")
	}

	if c.QuotePerOrder <= 0 {
		return fmt.Errorf("quote_per_order must be positive")
	}

	if c.K < 0 {
		return fmt.Errorf("k must be non-negative")
	}

	if c.MaxSkewBps < 0 {
		return fmt.Errorf("max_skew_bps must be non-negative")
	}

	if c.MinOffsetBps < 0 {
		return fmt.Errorf("min_offset_bps must be non-negative")
	}

	if c.RefreshJitterPct < 0 || c.RefreshJitterPct > 1 {
		return fmt.Errorf("refresh_jitter_pct must be between 0 and 1")
	}

	return nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if floatVal, err := strconv.ParseFloat(value, 64); err == nil {
			return floatVal
		}
	}
	return defaultValue
}

type ReplaceThresholds struct {
	RepriceThresholdBps int     `json:"reprice_threshold_bps"` // Reprice if mid moves by this many bps
	InvDevThreshold     float64 `json:"inv_dev_threshold"`     // Replace if inventory deviation changes by this amount
	MaxOrderAgeSec      int     `json:"max_order_age_sec"`     // Maximum order age before replacement
}

// RiskThresholds defines conditions that trigger RISK mode
type RiskThresholds struct {
	TtfFastSec       float64 `json:"ttf_fast_sec"`        // Fast time-to-fill threshold (seconds)
	FillSpikePerMin  float64 `json:"fill_spike_per_min"`  // Fill spike threshold (fills per minute)
	ImbHigh          float64 `json:"imb_high"`            // High imbalance threshold
	ImbLow           float64 `json:"imb_low"`             // Low imbalance threshold
	DriftFastPerHour float64 `json:"drift_fast_per_hour"` // Fast drift threshold (per hour)
}

// RiskActions defines how to modify behavior in RISK mode
type RiskActions struct {
	RiskSpreadMult  float64 `json:"risk_spread_mult"`  // Multiply spreads by this factor in RISK mode
	RiskSizeMult    float64 `json:"risk_size_mult"`    // Multiply sizes by this factor in RISK mode
	RiskRefreshMult float64 `json:"risk_refresh_mult"` // Multiply refresh interval by this factor in RISK mode
}
