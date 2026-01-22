package gate

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/types"

	"github.com/gorilla/websocket"
)

const (
	wsPingInterval = 30 * time.Second
)

// WSConnection manages the WebSocket connection for Gate.io user streams
type WSConnection struct {
	conn         *websocket.Conn
	apiKey       string
	apiSecret    string
	handlers     exchange.UserStreamHandlers
	currencyPair string // e.g., "BTC_USDT"
}

// NewWSConnection creates a new WebSocket connection
func NewWSConnection(apiKey, apiSecret, currencyPair string, handlers exchange.UserStreamHandlers) (*WSConnection, error) {
	log.Printf("Connecting to Gate.io WebSocket: %s", wsBaseURL)

	conn, _, err := websocket.DefaultDialer.Dial(wsBaseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to websocket: %w", err)
	}

	log.Printf("Gate.io WebSocket connection established for %s", currencyPair)

	return &WSConnection{
		conn:         conn,
		apiKey:       apiKey,
		apiSecret:    apiSecret,
		handlers:     handlers,
		currencyPair: currencyPair,
	}, nil
}

// generateWSSignature creates signature for WebSocket authentication
func (ws *WSConnection) generateWSSignature(channel string, timestamp int64) string {
	message := fmt.Sprintf("channel=%s&event=subscribe&time=%d", channel, timestamp)
	mac := hmac.New(sha512.New, []byte(ws.apiSecret))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

// Start starts the WebSocket connection and handles messages
func (ws *WSConnection) Start(ctx context.Context) error {
	defer ws.conn.Close()

	// Set read/write deadlines
	ws.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	ws.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))

	// Set pong handler
	ws.conn.SetPongHandler(func(string) error {
		ws.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	// Subscribe to channels
	if err := ws.subscribe(); err != nil {
		return fmt.Errorf("failed to subscribe: %w", err)
	}

	// Start ping ticker
	pingTicker := time.NewTicker(wsPingInterval)
	defer pingTicker.Stop()

	// Channel for errors
	errChan := make(chan error, 1)

	// Read messages in goroutine
	go func() {
		for {
			_, message, err := ws.conn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}

			ws.conn.SetReadDeadline(time.Now().Add(60 * time.Second))

			if err := ws.handleMessage(message); err != nil {
				log.Printf("Error handling message: %v", err)
			}
		}
	}()

	// Main loop
	for {
		select {
		case <-ctx.Done():
			return ws.conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		case err := <-errChan:
			return err
		case <-pingTicker.C:
			ws.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			// Gate.io uses channel ping
			pingMsg := map[string]interface{}{
				"time":    time.Now().Unix(),
				"channel": "spot.ping",
			}
			if err := ws.conn.WriteJSON(pingMsg); err != nil {
				return err
			}
		}
	}
}

// subscribe sends subscription messages for user stream channels
func (ws *WSConnection) subscribe() error {
	timestamp := time.Now().Unix()

	// Channels to subscribe (private channels require payload with currency pairs)
	channels := []string{"spot.orders", "spot.usertrades", "spot.balances"}

	for _, channel := range channels {
		signature := ws.generateWSSignature(channel, timestamp)

		subMsg := map[string]interface{}{
			"time":    timestamp,
			"channel": channel,
			"event":   "subscribe",
			"payload": []string{ws.currencyPair}, // Required: currency pair(s) to subscribe
			"auth": map[string]string{
				"method": "api_key",
				"KEY":    ws.apiKey,
				"SIGN":   signature,
			},
		}

		ws.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := ws.conn.WriteJSON(subMsg); err != nil {
			return fmt.Errorf("failed to subscribe to %s: %w", channel, err)
		}

		log.Printf("Subscribed to Gate.io channel: %s [%s]", channel, ws.currencyPair)
	}

	return nil
}

// handleMessage processes incoming WebSocket messages
func (ws *WSConnection) handleMessage(message []byte) error {
	var msg WSMessage
	if err := json.Unmarshal(message, &msg); err != nil {
		return fmt.Errorf("failed to unmarshal message: %w", err)
	}

	// Handle subscription confirmations
	if msg.Event == "subscribe" {
		log.Printf("[Gate.io WS] Subscription confirmed: %s", msg.Channel)
		return nil
	}

	// Handle pong
	if msg.Channel == "spot.pong" {
		return nil
	}

	// Handle errors
	if msg.Error != nil {
		log.Printf("[Gate.io WS] Error: code=%d, message=%s", msg.Error.Code, msg.Error.Message)
		return nil
	}

	// Handle update events
	if msg.Event != "update" {
		log.Printf("[Gate.io WS] Unknown event: %s, channel: %s", msg.Event, msg.Channel)
		return nil
	}

	switch msg.Channel {
	case "spot.orders":
		return ws.handleOrderUpdate(msg.Result)
	case "spot.usertrades":
		return ws.handleUserTrade(msg.Result)
	case "spot.balances":
		return ws.handleBalanceUpdate(msg.Result)
	}

	return nil
}

// handleOrderUpdate processes order update events
func (ws *WSConnection) handleOrderUpdate(result interface{}) error {
	if ws.handlers.OnOrderUpdate == nil {
		return nil
	}

	// Result is an array of order updates
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return err
	}

	var orders []WSOrderUpdate
	if err := json.Unmarshal(resultBytes, &orders); err != nil {
		return fmt.Errorf("failed to unmarshal order update: %w", err)
	}

	for _, order := range orders {
		price, _ := strconv.ParseFloat(order.Price, 64)
		amount, _ := strconv.ParseFloat(order.Amount, 64)
		left, _ := strconv.ParseFloat(order.Left, 64)
		filledTotal, _ := strconv.ParseFloat(order.FilledTotal, 64)
		executedQty := amount - left

		updateTime, _ := strconv.ParseInt(order.UpdateTime, 10, 64)

		ws.handlers.OnOrderUpdate(&types.OrderEvent{
			OrderID:            order.ID,
			ClientOrderID:      strings.TrimPrefix(order.Text, "t-"),
			Symbol:             convertSymbolBack(order.CurrencyPair),
			Side:               strings.ToUpper(order.Side),
			Type:               strings.ToUpper(order.Type),
			Status:             convertOrderStatus(order.Status),
			Price:              price,
			Quantity:           amount,
			ExecutedQty:        executedQty,
			CumulativeQuoteQty: filledTotal,
			Timestamp:          time.Unix(updateTime, 0),
		})
	}

	return nil
}

// handleUserTrade processes trade/fill events
func (ws *WSConnection) handleUserTrade(result interface{}) error {
	if ws.handlers.OnFill == nil {
		return nil
	}

	resultBytes, err := json.Marshal(result)
	if err != nil {
		return err
	}

	var trades []WSUserTrade
	if err := json.Unmarshal(resultBytes, &trades); err != nil {
		return fmt.Errorf("failed to unmarshal user trade: %w", err)
	}

	for _, trade := range trades {
		price, _ := strconv.ParseFloat(trade.Price, 64)
		amount, _ := strconv.ParseFloat(trade.Amount, 64)
		fee, _ := strconv.ParseFloat(trade.Fee, 64)

		ws.handlers.OnFill(&types.FillEvent{
			OrderID:         trade.OrderID,
			ClientOrderID:   strings.TrimPrefix(trade.Text, "t-"),
			Symbol:          convertSymbolBack(trade.CurrencyPair),
			Side:            strings.ToUpper(trade.Side),
			Price:           price,
			Quantity:        amount,
			Commission:      fee,
			CommissionAsset: trade.FeeCurrency,
			TradeID:         strconv.FormatInt(trade.ID, 10),
			Timestamp:       time.Unix(trade.CreateTime, 0),
		})
	}

	return nil
}

// handleBalanceUpdate processes balance update events
func (ws *WSConnection) handleBalanceUpdate(result interface{}) error {
	if ws.handlers.OnAccountUpdate == nil {
		return nil
	}

	resultBytes, err := json.Marshal(result)
	if err != nil {
		return err
	}

	var updates []WSBalanceUpdate
	if err := json.Unmarshal(resultBytes, &updates); err != nil {
		return fmt.Errorf("failed to unmarshal balance update: %w", err)
	}

	// Group by timestamp to create a single account event
	balances := make([]types.Balance, 0, len(updates))
	var latestTime time.Time

	for _, update := range updates {
		available, _ := strconv.ParseFloat(update.Available, 64)
		total, _ := strconv.ParseFloat(update.Total, 64)
		locked := total - available

		balances = append(balances, types.Balance{
			Asset:  update.Currency,
			Free:   available,
			Locked: locked,
		})

		// Parse timestamp
		ts, _ := strconv.ParseInt(update.Timestamp, 10, 64)
		t := time.Unix(ts, 0)
		if t.After(latestTime) {
			latestTime = t
		}
	}

	if len(balances) > 0 {
		ws.handlers.OnAccountUpdate(&types.AccountEvent{
			Balances:  balances,
			Timestamp: latestTime,
		})
	}

	return nil
}

// Close closes the WebSocket connection
func (ws *WSConnection) Close() error {
	if ws.conn == nil {
		return nil
	}

	deadline := time.Now().Add(5 * time.Second)
	ws.conn.SetWriteDeadline(deadline)

	err := ws.conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
	)
	if err != nil {
		log.Printf("Failed to send close message: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	return ws.conn.Close()
}
