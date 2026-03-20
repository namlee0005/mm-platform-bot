package bybit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/types"

	"github.com/gorilla/websocket"
)

const (
	// Bybit WebSocket endpoint for private streams
	wsPrivateURL = "wss://stream.bybit.com/v5/private"
	pingInterval = 20 * time.Second
)

// WSConnection manages the WebSocket connection for user streams
type WSConnection struct {
	conn      *websocket.Conn
	apiKey    string
	apiSecret string
	handlers  exchange.UserStreamHandlers
}

// NewWSConnection creates a new WebSocket connection
func NewWSConnection(apiKey, apiSecret string, handlers exchange.UserStreamHandlers) (*WSConnection, error) {
	log.Printf("Connecting to Bybit WebSocket: %s", wsPrivateURL)

	conn, _, err := websocket.DefaultDialer.Dial(wsPrivateURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to websocket: %w", err)
	}

	log.Printf("WebSocket connection established")

	return &WSConnection{
		conn:      conn,
		apiKey:    apiKey,
		apiSecret: apiSecret,
		handlers:  handlers,
	}, nil
}

// Start starts the WebSocket connection and handles messages
func (ws *WSConnection) Start(ctx context.Context) error {
	defer ws.conn.Close()

	// Set read/write deadlines
	if err := ws.conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
		return err
	}
	if err := ws.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}

	// Set pong handler to update read deadline
	ws.conn.SetPongHandler(func(string) error {
		return ws.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})

	// Authenticate first
	if err := ws.authenticate(); err != nil {
		return fmt.Errorf("failed to authenticate: %w", err)
	}

	// Subscribe to channels after auth
	if err := ws.subscribe(); err != nil {
		return fmt.Errorf("failed to subscribe to channels: %w", err)
	}

	// Start ping ticker
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	// Channel for errors
	errChan := make(chan error, 1)

	// Read messages in a goroutine
	go func() {
		for {
			_, message, err := ws.conn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}

			// Update read deadline on each message
			if err := ws.conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
				errChan <- err
				return
			}

			if err := ws.handleMessage(message); err != nil {
				log.Printf("Error handling message: %v", err)
				if ws.handlers.OnError != nil {
					ws.handlers.OnError(err)
				}
			}
		}
	}()

	// Main loop
	for {
		select {
		case <-ctx.Done():
			return ws.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		case err := <-errChan:
			return err
		case <-pingTicker.C:
			if err := ws.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				return err
			}
			// Bybit uses JSON ping
			pingMsg := map[string]string{"op": "ping"}
			if err := ws.conn.WriteJSON(pingMsg); err != nil {
				return err
			}
		}
	}
}

// authenticate sends authentication message to Bybit WebSocket
func (ws *WSConnection) authenticate() error {
	// Bybit auth: sign = HMAC_SHA256(expires + api_key)
	expires := time.Now().UnixMilli() + 10000 // 10 seconds from now
	expiresStr := strconv.FormatInt(expires, 10)

	signStr := "GET/realtime" + expiresStr
	mac := hmac.New(sha256.New, []byte(ws.apiSecret))
	mac.Write([]byte(signStr))
	signature := hex.EncodeToString(mac.Sum(nil))

	authMsg := map[string]interface{}{
		"op":   "auth",
		"args": []interface{}{ws.apiKey, expires, signature},
	}

	log.Printf("Authenticating WebSocket connection...")
	if err := ws.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}

	if err := ws.conn.WriteJSON(authMsg); err != nil {
		return fmt.Errorf("failed to send auth message: %w", err)
	}

	// Read auth response
	_, message, err := ws.conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("failed to read auth response: %w", err)
	}

	var authResp WSAuthMessage
	if err := json.Unmarshal(message, &authResp); err != nil {
		return fmt.Errorf("failed to unmarshal auth response: %w", err)
	}

	if !authResp.Success {
		return fmt.Errorf("authentication failed: %s", authResp.RetMsg)
	}

	log.Printf("WebSocket authentication successful")
	return nil
}

// subscribe sends subscription message to Bybit WebSocket
func (ws *WSConnection) subscribe() error {
	subscribeMsg := map[string]interface{}{
		"op": "subscribe",
		"args": []string{
			"order",     // Order updates
			"execution", // Trade executions
			"wallet",    // Wallet balance updates
		},
	}

	log.Printf("Subscribing to user stream channels...")
	if err := ws.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}

	if err := ws.conn.WriteJSON(subscribeMsg); err != nil {
		return fmt.Errorf("failed to send subscribe message: %w", err)
	}

	log.Println("Subscription message sent successfully")
	return nil
}

// handleMessage processes incoming WebSocket messages
func (ws *WSConnection) handleMessage(message []byte) error {
	// Try to determine message type
	var baseMsg map[string]interface{}
	if err := json.Unmarshal(message, &baseMsg); err != nil {
		return fmt.Errorf("failed to unmarshal message: %w", err)
	}

	// Check for pong response
	if op, ok := baseMsg["op"].(string); ok && op == "pong" {
		return nil
	}

	// Check for subscription response
	if op, ok := baseMsg["op"].(string); ok && op == "subscribe" {
		if success, ok := baseMsg["success"].(bool); ok && !success {
			log.Printf("Subscription failed: %s", string(message))
		}
		return nil
	}

	// Handle topic-based messages
	topic, ok := baseMsg["topic"].(string)
	if !ok {
		return nil // Unknown message type
	}

	switch topic {
	case "order":
		return ws.handleOrderUpdate(message)
	case "execution":
		return ws.handleExecutionUpdate(message)
	case "wallet":
		return ws.handleWalletUpdate(message)
	}

	return nil
}

// handleOrderUpdate processes order update events
func (ws *WSConnection) handleOrderUpdate(message []byte) error {
	if ws.handlers.OnOrderUpdate == nil {
		return nil
	}

	var update WSOrderUpdate
	if err := json.Unmarshal(message, &update); err != nil {
		return fmt.Errorf("failed to unmarshal order update: %w", err)
	}

	for _, data := range update.Data {
		// Only process spot orders
		if data.Category != "spot" {
			continue
		}

		ws.handlers.OnOrderUpdate(&types.OrderEvent{
			OrderID:            data.OrderID,
			ClientOrderID:      data.OrderLinkID,
			Symbol:             data.Symbol,
			Side:               mapSide(data.Side),
			Type:               mapOrderType(data.OrderType),
			Status:             mapOrderStatus(data.OrderStatus),
			Price:              parseFloat(data.Price),
			Quantity:           parseFloat(data.Qty),
			ExecutedQty:        parseFloat(data.CumExecQty),
			CumulativeQuoteQty: parseFloat(data.CumExecValue),
			Timestamp:          parseTimestamp(data.UpdateTime),
		})
	}

	return nil
}

// handleExecutionUpdate processes execution/trade events
func (ws *WSConnection) handleExecutionUpdate(message []byte) error {
	if ws.handlers.OnFill == nil {
		return nil
	}

	var update WSExecutionUpdate
	if err := json.Unmarshal(message, &update); err != nil {
		return fmt.Errorf("failed to unmarshal execution update: %w", err)
	}

	for _, data := range update.Data {
		// Only process spot executions
		if data.Category != "spot" {
			continue
		}

		ws.handlers.OnFill(&types.FillEvent{
			OrderID:         data.OrderID,
			ClientOrderID:   data.OrderLinkID,
			Symbol:          data.Symbol,
			Side:            mapSide(data.Side),
			Price:           parseFloat(data.ExecPrice),
			Quantity:        parseFloat(data.ExecQty),
			Commission:      parseFloat(data.ExecFee),
			CommissionAsset: data.FeeCurrency,
			TradeID:         data.ExecID,
			Timestamp:       parseTimestamp(data.ExecTime),
		})
	}

	return nil
}

// handleWalletUpdate processes wallet balance updates
func (ws *WSConnection) handleWalletUpdate(message []byte) error {
	log.Printf("[Bybit WS] Received wallet update: %s", string(message))

	if ws.handlers.OnAccountUpdate == nil {
		return nil
	}

	var update WSWalletUpdate
	if err := json.Unmarshal(message, &update); err != nil {
		return fmt.Errorf("failed to unmarshal wallet update: %w", err)
	}

	for _, data := range update.Data {
		var balances []types.Balance
		for _, coin := range data.Coin {
			free := parseFloat(coin.Free)
			locked := parseFloat(coin.Locked)
			walletBal := parseFloat(coin.WalletBalance)
			// If Free is empty, calculate from walletBalance - locked
			if free == 0 && walletBal > 0 {
				free = walletBal - locked
			}
			log.Printf("[Bybit WS] Wallet %s: free=%.8f, locked=%.8f, wallet=%.8f",
				coin.Coin, free, locked, walletBal)
			balances = append(balances, types.Balance{
				Asset:  coin.Coin,
				Free:   free,
				Locked: locked,
			})
		}

		if len(balances) > 0 {
			ws.handlers.OnAccountUpdate(&types.AccountEvent{
				Balances:  balances,
				Timestamp: time.Now(),
			})
		}
	}

	return nil
}

// parseTimestamp converts string timestamp to time.Time
func parseTimestamp(s string) time.Time {
	if s == "" {
		return time.Now()
	}
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Now()
	}
	return time.UnixMilli(ms)
}

// Close closes the WebSocket connection gracefully
func (ws *WSConnection) Close() error {
	if ws.conn == nil {
		return nil
	}

	// Send close message
	deadline := time.Now().Add(5 * time.Second)
	ws.conn.SetWriteDeadline(deadline)

	err := ws.conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
	)
	if err != nil {
		log.Printf("Failed to send close message: %v", err)
	}

	// Wait a bit for close handshake
	time.Sleep(100 * time.Millisecond)

	// Close the connection
	return ws.conn.Close()
}
