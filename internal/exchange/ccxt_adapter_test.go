package exchange

import (
	"testing"
)

func TestConvertToCCXTSymbol(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"USDT pair", "BTCUSDT", "BTC/USDT"},
		{"USDC pair", "ETHUSDC", "ETH/USDC"},
		{"BTC quote", "ETHBTC", "ETH/BTC"},
		{"Already CCXT format", "BTC/USDT", "BTC/USDT"},
		{"USD pair", "BTCUSD", "BTC/USD"},
		{"BUSD pair", "BTCBUSD", "BTC/BUSD"},
		{"Long base", "SHIBUSDT", "SHIB/USDT"},
		{"Short base", "AMIUSDT", "AMI/USDT"},
		{"EUR pair", "BTCEUR", "BTC/EUR"},
		{"BNB quote", "ETHBNB", "ETH/BNB"},
		{"No match - return as is", "UNKNOWN", "UNKNOWN"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertToCCXTSymbol(tt.input)
			if result != tt.expected {
				t.Errorf("convertToCCXTSymbol(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestDerefString(t *testing.T) {
	tests := []struct {
		name     string
		input    *string
		expected string
	}{
		{"nil pointer", nil, ""},
		{"empty string", strPtr(""), ""},
		{"non-empty string", strPtr("hello"), "hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := derefString(tt.input)
			if result != tt.expected {
				t.Errorf("derefString() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestDerefFloat(t *testing.T) {
	tests := []struct {
		name     string
		input    *float64
		expected float64
	}{
		{"nil pointer", nil, 0},
		{"zero value", floatPtr(0), 0},
		{"positive value", floatPtr(123.456), 123.456},
		{"negative value", floatPtr(-99.99), -99.99},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := derefFloat(tt.input)
			if result != tt.expected {
				t.Errorf("derefFloat() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestDerefInt64(t *testing.T) {
	tests := []struct {
		name     string
		input    *int64
		expected int64
	}{
		{"nil pointer", nil, 0},
		{"zero value", int64Ptr(0), 0},
		{"positive value", int64Ptr(1234567890), 1234567890},
		{"negative value", int64Ptr(-1000), -1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := derefInt64(tt.input)
			if result != tt.expected {
				t.Errorf("derefInt64() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestMapCCXTStatus(t *testing.T) {
	tests := []struct {
		name     string
		input    *string
		expected string
	}{
		{"nil status", nil, "UNKNOWN"},
		{"open status", strPtr("open"), "NEW"},
		{"closed status", strPtr("closed"), "FILLED"},
		{"canceled status", strPtr("canceled"), "CANCELED"},
		{"cancelled status (UK spelling)", strPtr("cancelled"), "CANCELED"},
		{"expired status", strPtr("expired"), "EXPIRED"},
		{"rejected status", strPtr("rejected"), "REJECTED"},
		{"unknown status", strPtr("something_else"), "SOMETHING_ELSE"},
		{"OPEN uppercase", strPtr("OPEN"), "NEW"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mapCCXTStatus(tt.input)
			if result != tt.expected {
				t.Errorf("mapCCXTStatus(%v) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestSupportedExchanges(t *testing.T) {
	exchanges := SupportedExchanges()

	expected := []string{"bybit", "binance", "okx", "gate", "kucoin", "mexc", "htx", "bitget"}

	if len(exchanges) != len(expected) {
		t.Errorf("SupportedExchanges() returned %d exchanges, want %d", len(exchanges), len(expected))
	}

	for i, ex := range expected {
		if exchanges[i] != ex {
			t.Errorf("SupportedExchanges()[%d] = %q, want %q", i, exchanges[i], ex)
		}
	}
}

func TestNewCCXTExchange_UnsupportedExchange(t *testing.T) {
	_, err := NewCCXTExchange("unsupported_exchange", "key", "secret", "BTCUSDT", false)
	if err == nil {
		t.Error("NewCCXTExchange() expected error for unsupported exchange, got nil")
	}
}

func TestNewCCXTExchange_SupportedExchanges(t *testing.T) {
	exchanges := []string{"bybit", "binance", "okx", "gate", "kucoin", "mexc", "htx", "bitget"}

	for _, ex := range exchanges {
		t.Run(ex, func(t *testing.T) {
			adapter, err := NewCCXTExchange(ex, "test_key", "test_secret", "BTCUSDT", false)
			if err != nil {
				t.Errorf("NewCCXTExchange(%q) returned error: %v", ex, err)
				return
			}
			if adapter == nil {
				t.Errorf("NewCCXTExchange(%q) returned nil adapter", ex)
			}
		})
	}
}

// Helper functions for creating pointers
func strPtr(s string) *string {
	return &s
}

func floatPtr(f float64) *float64 {
	return &f
}

func int64Ptr(i int64) *int64 {
	return &i
}
