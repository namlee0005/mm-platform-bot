package mexc

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
	"net/url"
	"strconv"
	"sync"
	"time"

	"mm-platform-engine/internal/exchange"
)

// Client implements the Exchange interface for MEXC
type Client struct {
	apiKey     string
	apiSecret  string
	baseURL    string
	httpClient *http.Client

	// WebSocket related
	listenKeyMu   sync.RWMutex
	listenKey     string
	wsConn        *WSConnection
	streamRunning bool
}

// NewClient creates a new MEXC client
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
	// Initialize listen key for user stream
	var err error
	c.listenKey, err = c.createListenKey(ctx)
	if err != nil {
		return fmt.Errorf("failed to create listen key: %w", err)
	}

	// Start listen key keepalive
	go c.keepAliveListenKey(ctx)

	return nil
}

// Stop closes the client
func (c *Client) Stop(ctx context.Context) error {
	log.Println("Stopping MEXC client...")

	// Close WebSocket connection
	if c.wsConn != nil {
		log.Println("Closing WebSocket connection...")
		if err := c.wsConn.Close(); err != nil {
			log.Printf("Failed to close WebSocket: %v", err)
		}
		c.wsConn = nil
	}

	// Delete listen key to clean up server resources
	c.listenKeyMu.RLock()
	hasListenKey := c.listenKey != ""
	c.listenKeyMu.RUnlock()

	if hasListenKey {
		log.Println("Deleting listen key...")
		if err := c.deleteListenKey(ctx); err != nil {
			log.Printf("Failed to delete listen key: %v", err)
		} else {
			log.Println("✓ Listen key deleted successfully")
		}
	}

	log.Println("MEXC client stopped")
	return nil
}

// sign creates HMAC SHA256 signature for the query string
func (c *Client) sign(queryString string) string {
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(queryString))
	return hex.EncodeToString(mac.Sum(nil))
}

// doRequest performs an authenticated HTTP request
func (c *Client) doRequest(ctx context.Context, method, endpoint string, params url.Values, sign bool) ([]byte, error) {
	reqURL := c.baseURL + endpoint
	if sign {
		// Add timestamp
		timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
		params.Set("timestamp", timestamp)

		// Create signature
		queryString := params.Encode()
		signature := c.sign(queryString)
		params.Set("signature", signature)
	}

	// MEXC API: All parameters go in query string for all methods (GET, POST, PUT, DELETE)
	if len(params) > 0 {
		reqURL += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, nil)
	if err != nil {
		return nil, err
	}

	// Set headers
	req.Header.Set("X-MEXC-APIKEY", c.apiKey)
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

	return body, nil
}

// GetAccount retrieves account information
func (c *Client) GetAccount(ctx context.Context) (*exchange.Account, error) {
	params := url.Values{}
	params.Set("recvWindow", "5000")

	body, err := c.doRequest(ctx, "GET", "/api/v3/account", params, true)
	if err != nil {
		return nil, err
	}

	var account Account
	if err := json.Unmarshal(body, &account); err != nil {
		return nil, err
	}

	// Convert to exchange.Account
	balances := make([]exchange.Balance, len(account.Balances))
	for i, b := range account.Balances {
		balances[i] = exchange.Balance{
			Asset:  b.Asset,
			Free:   b.Free,
			Locked: b.Locked,
		}
	}

	return &exchange.Account{Balances: balances}, nil
}

// SubscribeUserStream subscribes to user stream events
func (c *Client) SubscribeUserStream(ctx context.Context, handlers exchange.UserStreamHandlers) error {
	// Close existing connection if any
	if c.wsConn != nil {
		c.wsConn.Close()
		c.wsConn = nil
	}

	// Create a new listen key for each subscription (fresh connection)
	newKey, err := c.createListenKey(ctx)
	if err != nil {
		return fmt.Errorf("failed to create new listen key: %w", err)
	}

	// Update listen key with lock to prevent race with keepAlive
	c.listenKeyMu.Lock()
	c.listenKey = newKey
	c.listenKeyMu.Unlock()

	log.Printf("Created new listen key for WebSocket connection")

	wsConn, err := NewWSConnection(c.listenKey, handlers)
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
		c.streamRunning = false
	}()

	return nil
}
