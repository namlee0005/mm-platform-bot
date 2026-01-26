package config

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type TradingConfig struct {
	Type       string `json:"type" bson:"type"`
	Symbol     string `json:"symbol" bson:"symbol"`
	BaseAsset  string `json:"base_asset" bson:"base_asset"`
	QuoteAsset string `json:"quote_asset" bson:"quote_asset"`

	// Inventory target
	TargetRatio float64 `json:"target_ratio" bson:"target_ratio"` // Target inventory ratio (0.5 = 50/50)

	// Ladder configuration
	OffsetsBps    []int     `json:"offsets_bps" bson:"offsets_bps"`         // Offsets from mid in basis points (must be increasing)
	SizeMult      []float64 `json:"size_mult" bson:"size_mult"`             // Size multipliers for each level (must be non-increasing)
	QuotePerOrder float64   `json:"quote_per_order" bson:"quote_per_order"` // Base quote amount per order

	// Skew parameters
	Deadzone           float64 `json:"deadzone" bson:"deadzone"`                               // Inventory deviation deadzone (no skew if within)
	K                  float64 `json:"k" bson:"k"`                                             // Skew sensitivity factor
	MaxSkewBps         int     `json:"max_skew_bps" bson:"max_skew_bps"`                       // Maximum skew in basis points
	MinOffsetBps       int     `json:"min_offset_bps" bson:"min_offset_bps"`                   // Minimum offset from mid in basis points
	DSkewMaxBpsPerTick int     `json:"d_skew_max_bps_per_tick" bson:"d_skew_max_bps_per_tick"` // Maximum skew change per tick (rate limit)

	// Replace thresholds
	ReplaceThresholds ReplaceThresholds `json:"replace_thresholds" bson:"replace_thresholds"`

	// Risk thresholds
	RiskThresholds RiskThresholds `json:"risk_thresholds" bson:"risk_thresholds"`

	// Risk actions
	RiskActions RiskActions `json:"risk_actions" bson:"risk_actions"`

	// Refresh configuration
	RefreshBaseSec   int     `json:"refresh_base_sec" bson:"refresh_base_sec"`     // Base refresh interval in seconds
	RefreshJitterPct float64 `json:"refresh_jitter_pct" bson:"refresh_jitter_pct"` // Refresh jitter percentage (e.g., 0.15 = ±15%)
}

type Config struct {
	// User exchange key ID (for config reload)
	UserExchangeKeyID string

	// Exchange settings
	ExchangeName      string // "mexc", "gate", etc.
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
	MongoURI string
	MongoDB  string

	// HTTP server settings
	HTTPPort int

	// Operational settings
	LogLevel string
}

// Load reads configuration from environment variables and MongoDB
// Automatically loads from .env file if it exists
func Load() (*Config, error) {
	// Load .env file if it exists (optional)
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using system environment variables")
	} else {
		log.Println("Loaded configuration from .env file")
	}

	userExchangeKeyID := getEnv("USER_EXCHANGE_KEY_ID", "")
	if userExchangeKeyID == "" {
		return nil, errors.New("USER_EXCHANGE_KEY_ID is required")
	}

	log.Printf("mongoURI: %s\n", getEnv("MONGO_URI", "mongodb"))

	mongoURI := getEnv("MONGO_URI", "mongodb://localhost:27017")
	mongoDB := getEnv("MONGO_DB", "mm-platform")

	// Query MongoDB for user exchange key and exchange config
	userExchangeKey, exchange, err := loadUserExchangeKeyWithExchange(mongoURI, mongoDB, userExchangeKeyID)
	if err != nil {
		return nil, fmt.Errorf("failed to load config from MongoDB: %w", err)
	}

	// Check if key is active
	if !userExchangeKey.IsActive {
		return nil, errors.New("user exchange key is not active")
	}

	if userExchangeKey.IsDeleted {
		return nil, errors.New("user exchange key is deleted")
	}

	// Decrypt API keys
	masterKey := getEnv("APP_MASTER_KEY", "")
	if masterKey == "" {
		return nil, errors.New("APP_MASTER_KEY is required")
	}

	// Step 1: Decrypt user secret using master key
	userSecret, err := DecryptUserSecret(userExchangeKey.EncryptedUserSecret, masterKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt user secret: %w", err)
	}

	// Step 2: Decrypt API key using user secret
	apiKey, err := DecryptPrivateKey(userExchangeKey.APIKey, userSecret)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt API key: %w", err)
	}

	// Step 3: Decrypt API secret using user secret
	apiSecret, err := DecryptPrivateKey(userExchangeKey.APISecret, userSecret)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt API secret: %w", err)
	}

	log.Printf("REDIS_ADDR: %s\n", getEnv("REDIS_ADDR", "array"))

	log.Printf("Successfully decrypted API credentials for key: %s", userExchangeKey.KeyName)

	cfg := &Config{
		// User exchange key ID (for config reload)
		UserExchangeKeyID: userExchangeKeyID,

		// Exchange settings - decrypted
		ExchangeName:      exchange.Name,
		ExchangeAPIKey:    apiKey,
		ExchangeAPISecret: apiSecret,
		ExchangeBaseURL:   exchange.BaseURL,

		// Trading settings from MongoDB
		TradingConfig: userExchangeKey.Config,

		// Redis settings from env
		RedisAddr:     getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword: getEnv("REDIS_PASSWORD", ""),
		RedisDB:       getEnvInt("REDIS_DB", 0),

		// MongoDB settings
		MongoURI: mongoURI,
		MongoDB:  mongoDB,

		// HTTP server settings
		HTTPPort: getEnvInt("HTTP_PORT", 8080),

		// Operational settings
		LogLevel: getEnv("LOG_LEVEL", "info"),
	}

	return cfg, nil
}

// loadUserExchangeKeyWithExchange queries MongoDB to get the user exchange key and exchange configuration
func loadUserExchangeKeyWithExchange(mongoURI, dbName, keyID string) (*UserExchangeKey, *Exchange, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Connect to MongoDB
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to MongoDB: %w", err)
	}
	defer func() {
		if err := client.Disconnect(ctx); err != nil {
			log.Printf("Error disconnecting from MongoDB: %v", err)
		}
	}()

	// Ping to verify connection
	if err := client.Ping(ctx, nil); err != nil {
		return nil, nil, fmt.Errorf("failed to ping MongoDB: %w", err)
	}

	db := client.Database(dbName)

	// Parse the ObjectID
	objectID, err := primitive.ObjectIDFromHex(keyID)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid ObjectID format: %w", err)
	}

	// Query user_exchange_keys by _id
	var userExchangeKey UserExchangeKey
	err = db.Collection("user_exchange_keys").FindOne(ctx, bson.M{"_id": objectID}).Decode(&userExchangeKey)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil, fmt.Errorf("user exchange key not found with ID: %s", keyID)
		}
		return nil, nil, fmt.Errorf("failed to query user exchange key: %w", err)
	}

	log.Printf("Loaded user exchange key: %s (keyName: %s)", keyID, userExchangeKey.KeyName)

	// Query exchanges by exchangeId
	var exchange Exchange
	err = db.Collection("exchanges").FindOne(ctx, bson.M{"_id": userExchangeKey.ExchangeID}).Decode(&exchange)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil, fmt.Errorf("exchange not found with ID: %s", userExchangeKey.ExchangeID.Hex())
		}
		return nil, nil, fmt.Errorf("failed to query exchange: %w", err)
	}

	log.Printf("Loaded exchange: %s (baseUrl: %s)", exchange.Name, exchange.BaseURL)

	return &userExchangeKey, &exchange, nil
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
	RepriceThresholdBps int     `json:"reprice_threshold_bps" bson:"reprice_threshold_bps"` // Reprice if mid moves by this many bps
	InvDevThreshold     float64 `json:"inv_dev_threshold" bson:"inv_dev_threshold"`         // Replace if inventory deviation changes by this amount
	MaxOrderAgeSec      int     `json:"max_order_age_sec" bson:"max_order_age_sec"`         // Maximum order age before replacement
}

// RiskThresholds defines conditions that trigger RISK mode
type RiskThresholds struct {
	TtfFastSec       float64 `json:"ttf_fast_sec" bson:"ttf_fast_sec"`               // Fast time-to-fill threshold (seconds)
	FillSpikePerMin  float64 `json:"fill_spike_per_min" bson:"fill_spike_per_min"`   // Fill spike threshold (fills per minute)
	ImbHigh          float64 `json:"imb_high" bson:"imb_high"`                       // High imbalance threshold
	ImbLow           float64 `json:"imb_low" bson:"imb_low"`                         // Low imbalance threshold
	DriftFastPerHour float64 `json:"drift_fast_per_hour" bson:"drift_fast_per_hour"` // Fast drift threshold (per hour)
}

// RiskActions defines how to modify behavior in RISK mode
type RiskActions struct {
	RiskSpreadMult  float64 `json:"risk_spread_mult" bson:"risk_spread_mult"`   // Multiply spreads by this factor in RISK mode
	RiskSizeMult    float64 `json:"risk_size_mult" bson:"risk_size_mult"`       // Multiply sizes by this factor in RISK mode
	RiskRefreshMult float64 `json:"risk_refresh_mult" bson:"risk_refresh_mult"` // Multiply refresh interval by this factor in RISK mode
}

// UserExchangeKey represents the MongoDB document structure for user_exchange_keys collection
type UserExchangeKey struct {
	ID                  primitive.ObjectID  `bson:"_id,omitempty"`
	UserID              string              `bson:"userId"`
	ExchangeID          primitive.ObjectID  `bson:"exchangeId"`
	PairID              primitive.ObjectID  `bson:"pairId"`
	EncryptedUserSecret EncryptedUserSecret `bson:"encryptedUserSecret"`
	KeyName             string              `bson:"keyName"`
	APIKey              EncryptedData       `bson:"apiKey"`
	APISecret           EncryptedData       `bson:"apiSecret"`
	Passphrase          EncryptedData       `bson:"passphrase"`
	EnableSpot          bool                `bson:"enableSpot"`
	EnableFuture        bool                `bson:"enableFuture"`
	IsDeleted           bool                `bson:"isDeleted"`
	IsActive            bool                `bson:"isActive"`
	IsRunning           bool                `bson:"isRunning"`
	IsConfigUpdated     *bool               `bson:"isConfigUpdated,omitempty"`
	Config              TradingConfig       `bson:"config"`
	CreatedAt           time.Time           `bson:"createdAt"`
	UpdatedAt           time.Time           `bson:"updatedAt"`
}

// Exchange represents the MongoDB document structure for exchanges collection
type Exchange struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	Name      string             `bson:"name"`
	BaseURL   string             `bson:"baseUrl"`
	IsDeleted bool               `bson:"isDeleted"`
	CreatedAt time.Time          `bson:"createdAt"`
	UpdatedAt time.Time          `bson:"updatedAt"`
}
