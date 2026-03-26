package bybit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"mm-platform-engine/internal/exchange"
)

const (
	recvWindow = "5000"
)

// Client implements the Exchange interface for Bybit
type Client struct {
	apiKey     string
	apiSecret  string
	baseURL    string
	httpClient *http.Client

	// WebSocket related
	wsConn        *WSConnection
	wsMu          sync.RWMutex
	streamRunning bool
}

// NewClient creates a new Bybit client
func NewClient(apiKey, apiSecret, baseURL string) *Client {
	return &Client{
		apiKey:     apiKey,
		apiSecret:  apiSecret,
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// Start initializes the client
func (c *Client) Start(ctx context.Context) error {
	log.Println("Bybit client started")
	return nil
}

// Stop closes the client
func (c *Client) Stop(ctx context.Context) error {
	log.Println("Stopping Bybit client...")

	c.wsMu.Lock()
	defer c.wsMu.Unlock()

	if c.wsConn != nil {
		log.Println("Closing WebSocket connection...")
		if err := c.wsConn.Close(); err != nil {
			log.Printf("Failed to close WebSocket: %v", err)
		}
		c.wsConn = nil
	}

	log.Println("Bybit client stopped")
	return nil
}

// sign creates HMAC SHA256 signature for Bybit V5 API
// Signature = HMAC_SHA256(timestamp + api_key + recv_window + payload)
func (c *Client) sign(timestamp, payload string) string {
	signStr := timestamp + c.apiKey + recvWindow + payload
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(signStr))
	return hex.EncodeToString(mac.Sum(nil))
}

// doRequest performs an authenticated HTTP request
func (c *Client) doRequest(ctx context.Context, method, endpoint string, params map[string]string) ([]byte, error) {
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)

	var reqURL string
	var bodyReader io.Reader
	var payload string

	if method == "GET" || method == "DELETE" {
		// For GET/DELETE, params go in query string
		if len(params) > 0 {
			queryParts := make([]string, 0, len(params))
			for k, v := range params {
				queryParts = append(queryParts, k+"="+v)
			}
			payload = strings.Join(queryParts, "&")
			reqURL = c.baseURL + endpoint + "?" + payload
		} else {
			reqURL = c.baseURL + endpoint
		}
	} else {
		// For POST/PUT, params go in JSON body
		reqURL = c.baseURL + endpoint
		if len(params) > 0 {
			jsonBody, err := json.Marshal(params)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal request body: %w", err)
			}
			payload = string(jsonBody)
			bodyReader = strings.NewReader(payload)
		}
	}

	signature := c.sign(timestamp, payload)

	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return nil, err
	}

	// Set Bybit V5 auth headers
	req.Header.Set("X-BAPI-API-KEY", c.apiKey)
	req.Header.Set("X-BAPI-SIGN", signature)
	req.Header.Set("X-BAPI-TIMESTAMP", timestamp)
	req.Header.Set("X-BAPI-RECV-WINDOW", recvWindow)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error: status=%d body=%s", resp.StatusCode, string(body))
	}

	// Check Bybit retCode
	var baseResp APIResponse
	if err := json.Unmarshal(body, &baseResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if baseResp.RetCode != 0 {
		return nil, fmt.Errorf("Bybit API error: code=%d msg=%s", baseResp.RetCode, baseResp.RetMsg)
	}

	return body, nil
}

// doPublicRequest performs a public (unauthenticated) HTTP request
func (c *Client) doPublicRequest(ctx context.Context, method, endpoint string, params map[string]string) ([]byte, error) {
	var reqURL string

	if len(params) > 0 {
		queryParts := make([]string, 0, len(params))
		for k, v := range params {
			queryParts = append(queryParts, k+"="+v)
		}
		reqURL = c.baseURL + endpoint + "?" + strings.Join(queryParts, "&")
	} else {
		reqURL = c.baseURL + endpoint
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error: status=%d body=%s", resp.StatusCode, string(body))
	}

	// Check Bybit retCode
	var baseResp APIResponse
	if err := json.Unmarshal(body, &baseResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if baseResp.RetCode != 0 {
		return nil, fmt.Errorf("Bybit API error: code=%d msg=%s", baseResp.RetCode, baseResp.RetMsg)
	}

	return body, nil
}

// GetAccount retrieves account information
func (c *Client) GetAccount(ctx context.Context) (*exchange.Account, error) {
	// Try UNIFIED first, then SPOT if no balances found
	accountTypes := []string{"UNIFIED", "SPOT"}

	for _, accountType := range accountTypes {
		params := map[string]string{
			"accountType": accountType,
		}

		body, err := c.doRequest(ctx, "GET", "/v5/account/wallet-balance", params)
		if err != nil {
			log.Printf("[Bybit] GetAccount %s error: %v", accountType, err)
			continue
		}

		var resp WalletBalanceResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			log.Printf("[Bybit] GetAccount unmarshal error: %v", err)
			continue
		}

		var balances []exchange.Balance
		for _, account := range resp.Result.List {
			for _, coin := range account.Coin {
				walletBalance := parseFloat(coin.WalletBalance)
				locked := parseFloat(coin.Locked)
				free := walletBalance - locked
				if walletBalance > 0 {
					log.Printf("[Bybit] Balance %s: wallet=%.8f, locked=%.8f, free=%.8f",
						coin.Coin, walletBalance, locked, free)
					balances = append(balances, exchange.Balance{
						Asset:  coin.Coin,
						Free:   free,
						Locked: locked,
					})
				}
			}
		}

		if len(balances) > 0 {
			log.Printf("[Bybit] GetAccount: found %d balances using %s account", len(balances), accountType)
			return &exchange.Account{Balances: balances}, nil
		}
	}

	log.Printf("[Bybit] GetAccount: no balances found in any account type")
	return &exchange.Account{Balances: []exchange.Balance{}}, nil
}

// SubscribeUserStream subscribes to user stream events
func (c *Client) SubscribeUserStream(ctx context.Context, handlers exchange.UserStreamHandlers) error {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()

	// Close existing connection if any
	if c.wsConn != nil {
		c.wsConn.Close()
		c.wsConn = nil
	}

	wsConn, err := NewWSConnection(c.apiKey, c.apiSecret, handlers)
	if err != nil {
		return err
	}

	c.wsConn = wsConn
	c.streamRunning = true

	go func() {
		if err := wsConn.Start(ctx); err != nil {
			if handlers.OnError != nil {
				handlers.OnError(fmt.Errorf("user stream error: %w", err))
			}
		}
		c.wsMu.Lock()
		c.streamRunning = false
		c.wsMu.Unlock()
	}()

	return nil
}

// parseFloat converts string to float64
func parseFloat(s string) float64 {
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}
