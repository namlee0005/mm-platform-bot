package config

import (
	"os"
	"testing"
)

func TestGetEnv(t *testing.T) {
	tests := []struct {
		name         string
		key          string
		defaultValue string
		envValue     string
		want         string
	}{
		{
			name:         "returns env value when set",
			key:          "TEST_KEY",
			defaultValue: "default",
			envValue:     "env_value",
			want:         "env_value",
		},
		{
			name:         "returns default when env not set",
			key:          "NONEXISTENT_KEY",
			defaultValue: "default",
			envValue:     "",
			want:         "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue != "" {
				os.Setenv(tt.key, tt.envValue)
				defer os.Unsetenv(tt.key)
			}

			got := getEnv(tt.key, tt.defaultValue)
			if got != tt.want {
				t.Errorf("getEnv() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetEnvInt(t *testing.T) {
	tests := []struct {
		name         string
		key          string
		defaultValue int
		envValue     string
		want         int
	}{
		{
			name:         "returns parsed int when valid",
			key:          "TEST_INT",
			defaultValue: 10,
			envValue:     "42",
			want:         42,
		},
		{
			name:         "returns default when invalid",
			key:          "TEST_INT",
			defaultValue: 10,
			envValue:     "invalid",
			want:         10,
		},
		{
			name:         "returns default when not set",
			key:          "NONEXISTENT_INT",
			defaultValue: 10,
			envValue:     "",
			want:         10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue != "" {
				os.Setenv(tt.key, tt.envValue)
				defer os.Unsetenv(tt.key)
			}

			got := getEnvInt(tt.key, tt.defaultValue)
			if got != tt.want {
				t.Errorf("getEnvInt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTradingConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		config  TradingConfig
		wantErr bool
	}{
		{
			name: "valid config",
			config: TradingConfig{
				OffsetsBps:       []int{10, 20, 40},
				SizeMult:         []float64{1.0, 0.8, 0.6},
				TargetRatio:      0.5,
				QuotePerOrder:    10.0,
				K:                2.0,
				MaxSkewBps:       200,
				MinOffsetBps:     5,
				RefreshJitterPct: 0.15,
			},
			wantErr: false,
		},
		{
			name: "empty offsets",
			config: TradingConfig{
				OffsetsBps: []int{},
				SizeMult:   []float64{},
			},
			wantErr: true,
		},
		{
			name: "mismatched lengths",
			config: TradingConfig{
				OffsetsBps: []int{10, 20},
				SizeMult:   []float64{1.0},
			},
			wantErr: true,
		},
		{
			name: "non-increasing offsets",
			config: TradingConfig{
				OffsetsBps: []int{10, 20, 15},
				SizeMult:   []float64{1.0, 0.8, 0.6},
			},
			wantErr: true,
		},
		{
			name: "invalid target ratio",
			config: TradingConfig{
				OffsetsBps:  []int{10, 20},
				SizeMult:    []float64{1.0, 0.8},
				TargetRatio: 1.5,
			},
			wantErr: true,
		},
		{
			name: "negative quote per order",
			config: TradingConfig{
				OffsetsBps:    []int{10, 20},
				SizeMult:      []float64{1.0, 0.8},
				TargetRatio:   0.5,
				QuotePerOrder: -10.0,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("TradingConfig.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{
			name: "valid config",
			config: Config{
				ExchangeAPIKey:    "test_key",
				ExchangeAPISecret: "test_secret",
			},
			wantErr: false,
		},
		{
			name: "missing API key",
			config: Config{
				ExchangeAPIKey:    "",
				ExchangeAPISecret: "test_secret",
			},
			wantErr: true,
		},
		{
			name: "missing API secret",
			config: Config{
				ExchangeAPIKey:    "test_key",
				ExchangeAPISecret: "",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Config.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
