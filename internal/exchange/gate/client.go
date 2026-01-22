package gate

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"mm-platform-engine/internal/exchange"
)

const (
	defaultBaseURL = "https://api.gateio.ws/api/v4"
	wsBaseURL      = "wss://api.gateio.ws/ws/v4/"
)

// Client implements the Exchange interface for Gate.io
type Client struct {
	apiKey       string
	apiSecret    string
	baseURL      string
	httpClient   *http.Client
	currencyPair string // e.g., "BTC_USDT" for WebSocket subscriptions

	// WebSocket related
	wsConn        *WSConnection
	wsMu          sync.RWMutex
	streamRunning bool
}

// NewClient creates a new Gate.io client
// currencyPair should be in Gate.io format e.g., "BTC_USDT"
func NewClient(apiKey, apiSecret, baseURL, currencyPair string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		apiKey:       apiKey,
		apiSecret:    apiSecret,
		baseURL:      baseURL,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		currencyPair: currencyPair,
	}
}

// Start initializes the client
func (c *Client) Start(ctx context.Context) error {
	log.Println("Gate.io client started")
	return nil
}

// Stop closes the client
func (c *Client) Stop(ctx context.Context) error {
	log.Println("Stopping Gate.io client...")

	// Close WebSocket connection
	c.wsMu.Lock()
	if c.wsConn != nil {
		log.Println("Closing WebSocket connection...")
		if err := c.wsConn.Close(); err != nil {
			log.Printf("Failed to close WebSocket: %v", err)
		}
		c.wsConn = nil
	}
	c.wsMu.Unlock()

	log.Println("Gate.io client stopped")
	return nil
}

// generateSignature creates HMAC-SHA512 signature for Gate.io API v4
// Signature string format: METHOD\nPATH\nQUERY\nHASHED_PAYLOAD\nTIMESTAMP
func (c *Client) generateSignature(method, path, query, body string, timestamp int64) string {
	// Hash the body with SHA512 (empty string if no body)
	h := sha512.New()
	h.Write([]byte(body))
	hashedPayload := hex.EncodeToString(h.Sum(nil))

	// Build signature string
	signatureString := fmt.Sprintf("%s\n%s\n%s\n%s\n%d",
		method,
		path,
		query,
		hashedPayload,
		timestamp,
	)

	// Create HMAC-SHA512 signature
	mac := hmac.New(sha512.New, []byte(c.apiSecret))
	mac.Write([]byte(signatureString))
	return hex.EncodeToString(mac.Sum(nil))
}

// doRequest performs an authenticated HTTP request to Gate.io API
func (c *Client) doRequest(ctx context.Context, method, endpoint string, query string, body string) ([]byte, error) {
	// Build full URL
	reqURL := c.baseURL + endpoint
	if query != "" {
		reqURL += "?" + query
	}

	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return nil, err
	}

	// Generate timestamp and signature
	// Extract path from baseURL for signature (e.g., /api/v4 from https://api.gateio.ws/api/v4)
	timestamp := time.Now().Unix()
	basePath := ""
	if idx := strings.Index(c.baseURL, "//"); idx != -1 {
		rest := c.baseURL[idx+2:]
		if pathIdx := strings.Index(rest, "/"); pathIdx != -1 {
			basePath = rest[pathIdx:]
		}
	}
	signaturePath := basePath + endpoint
	signature := c.generateSignature(method, signaturePath, query, body, timestamp)

	// Set headers
	req.Header.Set("KEY", c.apiKey)
	req.Header.Set("SIGN", signature)
	req.Header.Set("Timestamp", fmt.Sprintf("%d", timestamp))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Check for API errors
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var apiErr APIError
		if json.Unmarshal(respBody, &apiErr) == nil && apiErr.Label != "" {
			return nil, fmt.Errorf("API error: %s - %s", apiErr.Label, apiErr.Message)
		}
		return nil, fmt.Errorf("API error: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// doPublicRequest performs a public (unauthenticated) HTTP request
func (c *Client) doPublicRequest(ctx context.Context, method, endpoint string, query string) ([]byte, error) {
	reqURL := c.baseURL + endpoint
	if query != "" {
		reqURL += "?" + query
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// SubscribeUserStream subscribes to user stream events via WebSocket
func (c *Client) SubscribeUserStream(ctx context.Context, handlers exchange.UserStreamHandlers) error {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()

	if c.currencyPair == "" {
		return fmt.Errorf("currency pair not set in client")
	}

	log.Printf("Subscribing to Gate.io user stream for %s", c.currencyPair)

	// Close existing connection if any
	if c.wsConn != nil {
		c.wsConn.Close()
		c.wsConn = nil
	}

	wsConn, err := NewWSConnection(c.apiKey, c.apiSecret, c.currencyPair, handlers)
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
