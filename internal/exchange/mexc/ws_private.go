package mexc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"mm-platform-engine/internal/exchange"
	pb "mm-platform-engine/internal/exchange/mexc/proto"
	"mm-platform-engine/internal/types"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

const (
	// MEXC WebSocket endpoint for user data streams
	// Official docs: wss://wbs-api.mexc.com/ws?listenKey=<listenKey>
	wsBaseURL    = "wss://wbs-api.mexc.com/ws"
	pingInterval = 30 * time.Second
)

// WSConnection manages the WebSocket connection for user streams
type WSConnection struct {
	conn      *websocket.Conn
	listenKey string
	handlers  exchange.UserStreamHandlers
}

// NewWSConnection creates a new WebSocket connection
func NewWSConnection(listenKey string, handlers exchange.UserStreamHandlers) (*WSConnection, error) {
	wsURL := fmt.Sprintf("%s?listenKey=%s", wsBaseURL, listenKey)

	log.Printf("🔌 Connecting to WebSocket: %s", wsBaseURL)
	keyPreview := listenKey
	if len(listenKey) > 16 {
		keyPreview = listenKey[:16] + "..."
	}
	log.Printf("🔑 Using listenKey: %s", keyPreview)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to websocket: %w", err)
	}

	log.Printf("✓ WebSocket connection established to %s", wsBaseURL)

	return &WSConnection{
		conn:      conn,
		listenKey: listenKey,
		handlers:  handlers,
	}, nil
}

// Start starts the WebSocket connection and handles messages
func (ws *WSConnection) Start(ctx context.Context) error {
	defer func(conn *websocket.Conn) {
		err := conn.Close()
		if err != nil {

		}
	}(ws.conn)

	// Set read/write deadlines
	err := ws.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	if err != nil {
		return err
	}
	err = ws.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err != nil {
		return err
	}

	// Set pong handler to update read deadline
	ws.conn.SetPongHandler(func(string) error {
		err := ws.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		if err != nil {
			return err
		}
		return nil
	})

	// Subscribe to user stream channels after connection
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
			err = ws.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			if err != nil {
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
			err := ws.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err != nil {
				return err
			}
			if err := ws.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return err
			}
		}
	}
}

// handleMessage processes incoming WebSocket messages
func (ws *WSConnection) handleMessage(message []byte) error {
	// LOG RAW MESSAGE for debugging
	log.Printf("🔍 [RAW MESSAGE] Length: %d bytes", len(message))

	// Check if message is printable (likely JSON) or binary (protobuf)
	isPrintable := true
	for _, b := range message {
		if b < 32 && b != '\n' && b != '\r' && b != '\t' {
			isPrintable = false
			break
		}
	}

	if isPrintable {
		log.Printf("📄 [JSON?] %s", string(message))
	} else {
		previewLen := 32
		if len(message) < previewLen {
			previewLen = len(message)
		}
		log.Printf("📦 [BINARY/PROTOBUF] First %d bytes (hex): %x", previewLen, message[:previewLen])
	}

	// Try to parse as JSON first (for subscription responses)
	var baseMsg map[string]interface{}
	if err := json.Unmarshal(message, &baseMsg); err == nil {
		// This is a JSON message (likely subscription response)
		return ws.handleJSONMessage(baseMsg, message)
	}

	// This is a protobuf message - parse it
	return ws.handleProtobufMessage(message)

	// Check if this is a subscription response
	if method, ok := baseMsg["msg"].(string); ok {
		if method == "SUBSCRIPTION" {
			log.Printf("✓ Subscription confirmed: %s", string(message))
			return nil
		}
	}

	// Check for error messages
	if code, ok := baseMsg["code"].(float64); ok {
		if code != 0 {
			log.Printf("❌ WebSocket error response: %s", string(message))
			return fmt.Errorf("websocket error: %s", string(message))
		}
		// code == 0 means success response
		log.Printf("✓ Success response: %s", string(message))
		return nil
	}

	eventType, ok := baseMsg["e"].(string)
	if !ok {
		// Not a user stream event, might be a system message
		log.Printf("📨 Received message (not an event): %s", string(message))
		return nil
	}

	switch eventType {
	case "outboundAccountPosition":
		return ws.handleAccountUpdate(message)
	case "executionReport":
		return ws.handleOrderUpdate(message)
	case "trade":
		// Handle deals/trade events from spot@private.deals.v3.api
		return ws.handleTradeEvent(message)
	default:
		log.Printf("⚠️  Unknown event type: %s, message: %s", eventType, string(message))
	}

	return nil
}

// handleAccountUpdate processes account update events
func (ws *WSConnection) handleAccountUpdate(message []byte) error {
	if ws.handlers.OnAccountUpdate == nil {
		return nil
	}

	var event struct {
		EventType string `json:"e"`
		EventTime int64  `json:"E"`
		Balances  []struct {
			Asset  string  `json:"a"`
			Free   float64 `json:"f,string"`
			Locked float64 `json:"l,string"`
		} `json:"B"`
	}

	if err := json.Unmarshal(message, &event); err != nil {
		return fmt.Errorf("failed to unmarshal account event: %w", err)
	}

	balances := make([]types.Balance, len(event.Balances))
	for i, b := range event.Balances {
		balances[i] = types.Balance{
			Asset:  b.Asset,
			Free:   b.Free,
			Locked: b.Locked,
		}
	}

	ws.handlers.OnAccountUpdate(&types.AccountEvent{
		Balances:  balances,
		Timestamp: time.UnixMilli(event.EventTime),
	})

	return nil
}

// handleOrderUpdate processes order update events
func (ws *WSConnection) handleOrderUpdate(message []byte) error {
	var event struct {
		EventType           string  `json:"e"`
		EventTime           int64   `json:"E"`
		Symbol              string  `json:"s"`
		ClientOrderID       string  `json:"c"`
		Side                string  `json:"S"`
		OrderType           string  `json:"o"`
		OrderStatus         string  `json:"X"`
		OrderID             string  `json:"i"`
		LastExecutedQty     float64 `json:"l,string"`
		CumulativeFilledQty float64 `json:"z,string"`
		LastExecutedPrice   float64 `json:"L,string"`
		Commission          float64 `json:"n,string"`
		CommissionAsset     string  `json:"N"`
		TransactionTime     int64   `json:"T"`
		TradeID             string  `json:"t"`
		Price               float64 `json:"p,string"`
		Quantity            float64 `json:"q,string"`
		CumulativeQuoteQty  float64 `json:"Z,string"`
	}

	if err := json.Unmarshal(message, &event); err != nil {
		return fmt.Errorf("failed to unmarshal order event: %w", err)
	}

	// Send order update
	if ws.handlers.OnOrderUpdate != nil {
		ws.handlers.OnOrderUpdate(&types.OrderEvent{
			OrderID:            event.OrderID,
			ClientOrderID:      event.ClientOrderID,
			Symbol:             event.Symbol,
			Side:               event.Side,
			Type:               event.OrderType,
			Status:             event.OrderStatus,
			Price:              event.Price,
			Quantity:           event.Quantity,
			ExecutedQty:        event.CumulativeFilledQty,
			CumulativeQuoteQty: event.CumulativeQuoteQty,
			Timestamp:          time.UnixMilli(event.EventTime),
		})
	}

	// Send fill event if there was a trade
	if event.LastExecutedQty > 0 && ws.handlers.OnFill != nil {
		ws.handlers.OnFill(&types.FillEvent{
			OrderID:         event.OrderID,
			ClientOrderID:   event.ClientOrderID,
			Symbol:          event.Symbol,
			Side:            event.Side,
			Price:           event.LastExecutedPrice,
			Quantity:        event.LastExecutedQty,
			Commission:      event.Commission,
			CommissionAsset: event.CommissionAsset,
			TradeID:         event.TradeID,
			Timestamp:       time.UnixMilli(event.TransactionTime),
		})
	}

	return nil
}

// handleTradeEvent processes trade/deal events from spot@private.deals.v3.api
func (ws *WSConnection) handleTradeEvent(message []byte) error {
	if ws.handlers.OnFill == nil {
		return nil
	}

	var event struct {
		EventType       string  `json:"e"`
		EventTime       int64   `json:"E"`
		Symbol          string  `json:"s"`
		TradeID         string  `json:"t"`
		OrderID         string  `json:"o"`
		ClientOrderID   string  `json:"c"`
		Price           float64 `json:"p,string"`
		Quantity        float64 `json:"q,string"`
		Side            string  `json:"S"`
		Commission      float64 `json:"n,string"`
		CommissionAsset string  `json:"N"`
		TransactionTime int64   `json:"T"`
	}

	if err := json.Unmarshal(message, &event); err != nil {
		return fmt.Errorf("failed to unmarshal trade event: %w", err)
	}

	ws.handlers.OnFill(&types.FillEvent{
		OrderID:         event.OrderID,
		ClientOrderID:   event.ClientOrderID,
		Symbol:          event.Symbol,
		Side:            event.Side,
		Price:           event.Price,
		Quantity:        event.Quantity,
		Commission:      event.Commission,
		CommissionAsset: event.CommissionAsset,
		TradeID:         event.TradeID,
		Timestamp:       time.UnixMilli(event.TransactionTime),
	})

	return nil
}

// handleJSONMessage processes JSON messages (subscription responses)
func (ws *WSConnection) handleJSONMessage(baseMsg map[string]interface{}, message []byte) error {
	// Check if this is a subscription response
	if method, ok := baseMsg["msg"].(string); ok {
		log.Printf("✓ Subscription confirmed: %s", method)
		return nil
	}

	// Check for error messages
	if code, ok := baseMsg["code"].(float64); ok {
		if code != 0 {
			log.Printf("❌ WebSocket error response: %s", string(message))
			return fmt.Errorf("websocket error: %s", string(message))
		}
		// code == 0 means success response
		log.Printf("✓ Success response: %s", string(message))
		return nil
	}

	log.Printf("📨 Unknown JSON message: %s", string(message))
	return nil
}

// handleProtobufMessage processes protobuf messages
func (ws *WSConnection) handleProtobufMessage(message []byte) error {
	// Parse PushDataV3ApiWrapper
	wrapper := &pb.PushDataV3ApiWrapper{}
	if err := proto.Unmarshal(message, wrapper); err != nil {
		return fmt.Errorf("failed to unmarshal protobuf wrapper: %w", err)
	}

	log.Printf("📦 [PROTOBUF] Channel: %s", wrapper.Channel)

	// Handle based on channel type
	switch wrapper.Channel {
	case "spot@private.account.v3.api.pb":
		return ws.handleProtobufAccountUpdate(wrapper)
	case "spot@private.orders.v3.api.pb":
		return ws.handleProtobufOrderUpdate(wrapper)
	case "spot@private.deals.v3.api.pb":
		return ws.handleProtobufDeal(wrapper)
	default:
		log.Printf("⚠️  Unknown channel: %s", wrapper.Channel)
		return nil
	}
}

// handleProtobufAccountUpdate processes account update events
func (ws *WSConnection) handleProtobufAccountUpdate(wrapper *pb.PushDataV3ApiWrapper) error {
	if ws.handlers.OnAccountUpdate == nil {
		return nil
	}

	accountData := wrapper.GetPrivateAccount()
	if accountData == nil {
		return fmt.Errorf("no account data in wrapper")
	}

	// Convert protobuf to internal types
	balances := []types.Balance{
		{
			Asset:  accountData.VcoinName,
			Free:   parseFloat(accountData.BalanceAmount),
			Locked: parseFloat(accountData.FrozenAmount),
		},
	}

	ws.handlers.OnAccountUpdate(&types.AccountEvent{
		Balances:  balances,
		Timestamp: time.UnixMilli(accountData.Time),
	})

	return nil
}

// handleProtobufOrderUpdate processes order update events
func (ws *WSConnection) handleProtobufOrderUpdate(wrapper *pb.PushDataV3ApiWrapper) error {
	orderData := wrapper.GetPrivateOrders()
	if orderData == nil {
		return fmt.Errorf("no order data in wrapper")
	}

	symbol := ""
	if wrapper.Symbol != nil {
		symbol = *wrapper.Symbol
	}

	// Send order update
	if ws.handlers.OnOrderUpdate != nil {
		ws.handlers.OnOrderUpdate(&types.OrderEvent{
			OrderID:            orderData.Id,
			ClientOrderID:      orderData.ClientId,
			Symbol:             symbol,
			Side:               getSide(orderData.TradeType),
			Type:               getOrderType(orderData.OrderType),
			Status:             getOrderStatus(orderData.Status),
			Price:              parseFloat(orderData.Price),
			Quantity:           parseFloat(orderData.Quantity),
			ExecutedQty:        parseFloat(orderData.CumulativeQuantity),
			CumulativeQuoteQty: parseFloat(orderData.CumulativeAmount),
			Timestamp:          time.UnixMilli(orderData.CreateTime),
		})
	}

	// Also trigger fill event if lastDealQuantity is set (order was partially/fully filled)
	// This handles the case where MEXC doesn't send separate deal events
	if ws.handlers.OnFill != nil && orderData.LastDealQuantity != nil {
		lastDealQty := parseFloat(*orderData.LastDealQuantity)
		if lastDealQty > 0 {
			// Use avgPrice for the fill price since we don't have lastDealPrice
			fillPrice := parseFloat(orderData.AvgPrice)
			if fillPrice == 0 {
				fillPrice = parseFloat(orderData.Price)
			}

			ws.handlers.OnFill(&types.FillEvent{
				OrderID:       orderData.Id,
				ClientOrderID: orderData.ClientId,
				Symbol:        symbol,
				Side:          getSide(orderData.TradeType),
				Price:         fillPrice,
				Quantity:      lastDealQty,
				// Note: TradeID and fee info not available in order updates
				// Use order ID + timestamp as pseudo-TradeID
				TradeID:   fmt.Sprintf("%s_%d", orderData.Id, orderData.CreateTime),
				Timestamp: time.UnixMilli(orderData.CreateTime),
			})
		}
	}

	return nil
}

// handleProtobufDeal processes trade/deal events
func (ws *WSConnection) handleProtobufDeal(wrapper *pb.PushDataV3ApiWrapper) error {
	if ws.handlers.OnFill == nil {
		return nil
	}

	dealData := wrapper.GetPrivateDeals()
	if dealData == nil {
		return fmt.Errorf("no deal data in wrapper")
	}

	symbol := ""
	if wrapper.Symbol != nil {
		symbol = *wrapper.Symbol
	}

	// Convert protobuf to internal types
	ws.handlers.OnFill(&types.FillEvent{
		OrderID:         dealData.OrderId,
		ClientOrderID:   dealData.ClientOrderId,
		Symbol:          symbol,
		Side:            getSide(dealData.TradeType),
		Price:           parseFloat(dealData.Price),
		Quantity:        parseFloat(dealData.Quantity),
		Commission:      parseFloat(dealData.FeeAmount),
		CommissionAsset: dealData.FeeCurrency,
		TradeID:         dealData.TradeId,
		Timestamp:       time.UnixMilli(dealData.Time),
	})

	return nil
}

// Helper functions
func parseFloat(s string) float64 {
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

func getSide(tradeType int32) string {
	if tradeType == 1 {
		return "BUY"
	}
	return "SELL"
}

func getOrderType(orderType int32) string {
	switch orderType {
	case 1:
		return "LIMIT"
	case 2:
		return "MARKET"
	default:
		return fmt.Sprintf("TYPE_%d", orderType)
	}
}

func getOrderStatus(status int32) string {
	switch status {
	case 1:
		return "NEW"
	case 2:
		return "FILLED"
	case 3:
		return "PARTIALLY_FILLED"
	case 4:
		return "CANCELED"
	case 5:
		return "PARTIALLY_CANCELED"
	default:
		return fmt.Sprintf("STATUS_%d", status)
	}
}

// subscribe sends subscription message to MEXC WebSocket
func (ws *WSConnection) subscribe() error {
	// Subscribe to user stream channels
	subscribeMsg := map[string]interface{}{
		"method": "SUBSCRIPTION",
		"params": []string{
			"spot@private.deals.v3.api.pb",
			"spot@private.account.v3.api.pb",
			"spot@private.orders.v3.api.pb",
		},
	}

	msgBytes, err := json.Marshal(subscribeMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal subscribe message: %w", err)
	}

	log.Printf("📤 Subscribing to user stream channels...")
	log.Printf("📝 Subscription message: %s", string(msgBytes))

	ws.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := ws.conn.WriteMessage(websocket.TextMessage, msgBytes); err != nil {
		return fmt.Errorf("failed to send subscribe message: %w", err)
	}

	log.Println("✓ Subscription message sent successfully")
	return nil
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
