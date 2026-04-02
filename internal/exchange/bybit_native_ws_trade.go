package exchange

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ────────────────────────────────────────────────────────────────────────────
// Types (wsTrade prefix avoids conflicts with bybit_native_adapter.go)
// ────────────────────────────────────────────────────────────────────────────

// wsTradeResp is the response envelope from the Bybit V5 trade WebSocket.
// Every operation (place/amend/cancel) returns this structure keyed by reqId.
type wsTradeResp struct {
	ReqID   string          `json:"reqId"`
	RetCode int             `json:"retCode"`
	RetMsg  string          `json:"retMsg"`
	Op      string          `json:"op"`
	Data    json.RawMessage `json:"data"`
}

// wsTradeOrderResult is the data payload for order.create / order.amend / order.cancel.
type wsTradeOrderResult struct {
	OrderID     string `json:"orderId"`
	OrderLinkID string `json:"orderLinkId"`
}

// wsTradeReqSeq is a per-process monotonic counter for unique trade WS request IDs.
var wsTradeReqSeq atomic.Int64

func wsTradeNextID() string {
	return fmt.Sprintf("wt%d", wsTradeReqSeq.Add(1))
}

// ────────────────────────────────────────────────────────────────────────────
// BybitWSTrade
// ────────────────────────────────────────────────────────────────────────────

// BybitWSTrade is a standalone Bybit V5 trade WebSocket client.
//
// It connects to wss://stream.bybit.com/v5/trade and provides authenticated
// order operations with sub-millisecond overhead over REST equivalents.
//
// Concurrency model:
//   - sync.Map for pending channels: zero lock contention — LoadAndDelete on
//     disjoint reqIds never blocks.
//   - connMu mutex: serializes all writes to the underlying conn. gorilla/websocket
//     allows one concurrent reader + one concurrent writer, but never two writers.
//   - conn field: always read/written under connMu so conn replacement is safe.
//   - readLoop / keepalive: both receive a conn snapshot at launch and exit when
//     that snapshot is no longer w.conn (detected under connMu in keepalive;
//     implicit in readLoop because the closed conn returns an error).
type BybitWSTrade struct {
	apiKey    string
	apiSecret string
	url       string

	conn    *websocket.Conn
	connMu  sync.Mutex // serializes all writes to conn
	pending sync.Map   // reqId → chan *wsTradeResp

	ctx    context.Context
	cancel context.CancelFunc

	connected atomic.Bool
}

// NewBybitWSTrade creates a trade WS client. Call Connect before any order op.
// url should be the Bybit V5 trade WS endpoint (prod or testnet).
func NewBybitWSTrade(apiKey, apiSecret string, sandbox bool) *BybitWSTrade {
	u := bybitWSTradeURL
	if sandbox {
		u = bybitTestWSTradeURL
	}
	return &BybitWSTrade{
		apiKey:    apiKey,
		apiSecret: apiSecret,
		url:       u,
	}
}

// ════════════════════════════════════════════════════════════════════════════
// Lifecycle
// ════════════════════════════════════════════════════════════════════════════

// Connect dials the trade WebSocket, authenticates, and starts the read loop
// and keepalive goroutine. The provided ctx controls the lifetime of all
// background workers — cancel it to shut down cleanly.
func (w *BybitWSTrade) Connect(ctx context.Context) error {
	w.ctx, w.cancel = context.WithCancel(ctx)
	return w.dialAndStart()
}

// Close shuts down the connection and unblocks all pending callers with an error.
func (w *BybitWSTrade) Close() error {
	if w.cancel != nil {
		w.cancel()
	}
	w.connected.Store(false)
	w.drainPending(fmt.Errorf("bybit trade WS closed"))

	w.connMu.Lock()
	conn := w.conn
	w.connMu.Unlock()

	if conn != nil {
		w.connMu.Lock()
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		w.connMu.Unlock()
		conn.Close()
	}

	log.Printf("[BYBIT-WS-TRADE] Closed")
	return nil
}

// IsConnected returns true when the WS is live and authenticated.
func (w *BybitWSTrade) IsConnected() bool { return w.connected.Load() }

// dialAndStart dials, authenticates, and launches goroutines.
// Safe to call during reconnect without touching w.ctx / w.cancel.
func (w *BybitWSTrade) dialAndStart() error {
	conn, _, err := websocket.DefaultDialer.Dial(w.url, nil)
	if err != nil {
		return fmt.Errorf("bybit trade WS dial: %w", err)
	}

	// Authenticate synchronously — read loop is not running yet, so we
	// read the auth ack directly on the connection.
	if err := w.authenticate(conn); err != nil {
		conn.Close()
		return fmt.Errorf("bybit trade WS auth: %w", err)
	}

	// Replace the active connection under connMu so keepalive can detect it.
	w.connMu.Lock()
	w.conn = conn
	w.connMu.Unlock()

	w.connected.Store(true)
	go w.readLoop(conn)
	go w.keepalive(conn)

	log.Printf("[BYBIT-WS-TRADE] Connected and authenticated (%s)", w.url)
	return nil
}

// ════════════════════════════════════════════════════════════════════════════
// Authentication
// ════════════════════════════════════════════════════════════════════════════

// authenticate sends an HMAC-SHA256 auth message and reads the ack.
// Signature: HMAC_SHA256(secret, "GET/realtime" + expires_ms)
// This runs before the read loop so the ack is read directly.
func (w *BybitWSTrade) authenticate(conn *websocket.Conn) error {
	expires := time.Now().UnixMilli() + 10000 // 10-second window
	signStr := "GET/realtime" + strconv.FormatInt(expires, 10)
	mac := hmac.New(sha256.New, []byte(w.apiSecret))
	mac.Write([]byte(signStr))
	sig := hex.EncodeToString(mac.Sum(nil))

	authMsg := map[string]interface{}{
		"reqId": wsTradeNextID(),
		"header": map[string]string{
			"X-BAPI-TIMESTAMP":   strconv.FormatInt(time.Now().UnixMilli(), 10),
			"X-BAPI-RECV-WINDOW": bybitRecvWindow,
		},
		"op":   "auth",
		"args": []interface{}{w.apiKey, expires, sig},
	}

	conn.SetWriteDeadline(time.Now().Add(bybitWSTimeout))
	if err := conn.WriteJSON(authMsg); err != nil {
		return fmt.Errorf("send auth: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(bybitWSTimeout))
	_, raw, err := conn.ReadMessage()
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		return fmt.Errorf("read auth ack: %w", err)
	}

	var resp wsTradeResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("parse auth ack: %w", err)
	}
	if resp.RetCode != 0 {
		return fmt.Errorf("auth rejected: code=%d msg=%s", resp.RetCode, resp.RetMsg)
	}
	return nil
}

// ════════════════════════════════════════════════════════════════════════════
// Background goroutines
// ════════════════════════════════════════════════════════════════════════════

// readLoop reads messages from conn and dispatches them to pending request channels.
// Takes a conn snapshot: exits when that specific connection dies, not when
// w.conn is replaced (reconnect launches a fresh readLoop for the new conn).
func (w *BybitWSTrade) readLoop(conn *websocket.Conn) {
	defer log.Printf("[BYBIT-WS-TRADE] Read loop exited")

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if w.ctx.Err() != nil {
				return // normal shutdown via Close()
			}
			log.Printf("[BYBIT-WS-TRADE] Read error: %v — reconnecting", err)
			w.connected.Store(false)
			w.drainPending(fmt.Errorf("disconnected: %w", err))
			go w.reconnect()
			return
		}

		var resp wsTradeResp
		if err := json.Unmarshal(raw, &resp); err != nil {
			log.Printf("[BYBIT-WS-TRADE] Unmarshal error: %v (raw=%s)", err, raw)
			continue
		}

		// Silently drop pong and auth acks (not reqId-keyed)
		if resp.Op == "pong" || resp.Op == "auth" {
			continue
		}

		// Route response to the waiting caller by reqId
		if resp.ReqID != "" {
			if val, ok := w.pending.LoadAndDelete(resp.ReqID); ok {
				if ch, ok := val.(chan *wsTradeResp); ok {
					ch <- &resp
				}
			}
		}
	}
}

// keepalive sends a JSON ping every bybitPingInterval to prevent server-side timeout.
// Scoped to the conn snapshot — exits automatically when that conn is replaced.
func (w *BybitWSTrade) keepalive(conn *websocket.Conn) {
	ticker := time.NewTicker(bybitPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			w.connMu.Lock()
			stale := w.conn != conn // conn was replaced by reconnect
			if !stale {
				conn.SetWriteDeadline(time.Now().Add(bybitWSTimeout))
			}
			var err error
			if !stale {
				err = conn.WriteJSON(map[string]string{"op": "ping"})
			}
			w.connMu.Unlock()

			if stale {
				return // this conn is no longer active
			}
			if err != nil {
				log.Printf("[BYBIT-WS-TRADE] Ping failed: %v", err)
				return
			}
		}
	}
}

// reconnect re-dials with exponential backoff until success or context cancellation.
func (w *BybitWSTrade) reconnect() {
	for attempt := 1; ; attempt++ {
		if w.ctx.Err() != nil {
			return
		}
		delay := time.Duration(attempt) * 2 * time.Second
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		log.Printf("[BYBIT-WS-TRADE] Reconnecting in %v (attempt %d)...", delay, attempt)
		time.Sleep(delay)
		if w.ctx.Err() != nil {
			return
		}
		if err := w.dialAndStart(); err != nil {
			log.Printf("[BYBIT-WS-TRADE] Reconnect failed: %v", err)
			continue
		}
		log.Printf("[BYBIT-WS-TRADE] Reconnected after %d attempt(s)", attempt)
		return
	}
}

// drainPending sends an error sentinel to all waiting callers so they unblock.
func (w *BybitWSTrade) drainPending(cause error) {
	errResp := &wsTradeResp{RetCode: -1, RetMsg: cause.Error()}
	w.pending.Range(func(key, value any) bool {
		if ch, ok := value.(chan *wsTradeResp); ok {
			select {
			case ch <- errResp:
			default:
			}
		}
		w.pending.Delete(key)
		return true
	})
}

// ════════════════════════════════════════════════════════════════════════════
// Core request-response mechanism
// ════════════════════════════════════════════════════════════════════════════

// send writes a trade WS request and blocks until the matching response arrives.
//
// Design: the response channel is registered in w.pending BEFORE the write so
// the readLoop can never deliver the response before the channel is ready.
// sync.Map ensures LoadAndDelete on different reqIds never contends — each
// in-flight request owns its own key.
func (w *BybitWSTrade) send(ctx context.Context, op string, args interface{}) (*wsTradeResp, error) {
	if !w.connected.Load() {
		return nil, fmt.Errorf("trade WS not connected (op=%s)", op)
	}

	reqID := wsTradeNextID()
	msg := map[string]interface{}{
		"reqId": reqID,
		"header": map[string]string{
			"X-BAPI-TIMESTAMP":   strconv.FormatInt(time.Now().UnixMilli(), 10),
			"X-BAPI-RECV-WINDOW": bybitRecvWindow,
		},
		"op":   op,
		"args": args,
	}

	// Buffer 1: the read loop can deliver the response even if the select
	// hasn't started yet, without blocking the read loop goroutine.
	ch := make(chan *wsTradeResp, 1)
	w.pending.Store(reqID, ch)

	w.connMu.Lock()
	w.conn.SetWriteDeadline(time.Now().Add(bybitWSTimeout))
	writeErr := w.conn.WriteJSON(msg)
	w.connMu.Unlock()

	if writeErr != nil {
		w.pending.Delete(reqID)
		return nil, fmt.Errorf("ws write (%s): %w", op, writeErr)
	}

	// Respect caller deadline if tighter than our default timeout
	timeout := bybitWSTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		w.pending.Delete(reqID)
		return nil, fmt.Errorf("timeout waiting for %s response (reqId=%s)", op, reqID)
	case <-ctx.Done():
		w.pending.Delete(reqID)
		return nil, ctx.Err()
	}
}

// ════════════════════════════════════════════════════════════════════════════
// Order operations
// ════════════════════════════════════════════════════════════════════════════

// PlaceOrderWs submits a new order via the trade WebSocket.
// Returns an Order with OrderID populated from the exchange response.
// The executor uses GTX (PostOnly) time-in-force for all maker orders.
func (w *BybitWSTrade) PlaceOrderWs(ctx context.Context, order *OrderRequest) (*Order, error) {
	args := []map[string]string{{
		"category":    bybitCategory,
		"symbol":      order.Symbol,
		"side":        bybitNativeToSide(order.Side),
		"orderType":   bybitNativeToOrderType(order.Type),
		"qty":         bybitNativeFormatFloat(order.Quantity),
		"timeInForce": bybitNativeToTIF(order.TimeInForce),
	}}
	if order.Type == "LIMIT" && order.Price > 0 {
		args[0]["price"] = bybitNativeFormatFloat(order.Price)
	}
	if order.ClientOrderID != "" {
		args[0]["orderLinkId"] = order.ClientOrderID
	}

	resp, err := w.send(ctx, "order.create", args)
	if err != nil {
		return nil, fmt.Errorf("PlaceOrderWs: %w", err)
	}
	if resp.RetCode != 0 {
		return nil, fmt.Errorf("PlaceOrderWs: code=%d msg=%s", resp.RetCode, resp.RetMsg)
	}

	var result wsTradeOrderResult
	if len(resp.Data) > 0 {
		_ = json.Unmarshal(resp.Data, &result)
	}

	return &Order{
		OrderID:       result.OrderID,
		ClientOrderID: result.OrderLinkID,
		Symbol:        order.Symbol,
		Side:          order.Side,
		Type:          order.Type,
		Price:         order.Price,
		Quantity:      order.Quantity,
		Status:        "NEW",
		Timestamp:     time.Now(),
	}, nil
}

// EditOrderWs amends an existing order's price and/or qty without cancel+place.
// Avoids the cancel latency — critical for tight spread adjustments.
func (w *BybitWSTrade) EditOrderWs(ctx context.Context, orderID, symbol string, newPrice, newQty float64) (*Order, error) {
	args := []map[string]string{{
		"category": bybitCategory,
		"symbol":   symbol,
		"orderId":  orderID,
		"price":    bybitNativeFormatFloat(newPrice),
		"qty":      bybitNativeFormatFloat(newQty),
	}}

	resp, err := w.send(ctx, "order.amend", args)
	if err != nil {
		return nil, fmt.Errorf("EditOrderWs: %w", err)
	}
	if resp.RetCode != 0 {
		return nil, fmt.Errorf("EditOrderWs: code=%d msg=%s", resp.RetCode, resp.RetMsg)
	}

	var result wsTradeOrderResult
	if len(resp.Data) > 0 {
		_ = json.Unmarshal(resp.Data, &result)
	}

	return &Order{
		OrderID:   result.OrderID,
		Symbol:    symbol,
		Price:     newPrice,
		Quantity:  newQty,
		Status:    "NEW",
		Timestamp: time.Now(),
	}, nil
}

// CancelOrderWs cancels a single open order.
// Returns nil for retCode 110001 (order already gone — idempotent).
func (w *BybitWSTrade) CancelOrderWs(ctx context.Context, symbol, orderID string) error {
	args := []map[string]string{{
		"category": bybitCategory,
		"symbol":   symbol,
		"orderId":  orderID,
	}}

	resp, err := w.send(ctx, "order.cancel", args)
	if err != nil {
		return fmt.Errorf("CancelOrderWs: %w", err)
	}
	// 110001 = order does not exist (already filled/canceled) — treat as success
	if resp.RetCode != 0 && resp.RetCode != 110001 {
		return fmt.Errorf("CancelOrderWs: code=%d msg=%s", resp.RetCode, resp.RetMsg)
	}
	return nil
}

// CancelAllOrdersWs cancels all open orders for the given symbol.
func (w *BybitWSTrade) CancelAllOrdersWs(ctx context.Context, symbol string) error {
	args := []map[string]string{{
		"category": bybitCategory,
		"symbol":   symbol,
	}}

	resp, err := w.send(ctx, "order.cancelAll", args)
	if err != nil {
		return fmt.Errorf("CancelAllOrdersWs: %w", err)
	}
	if resp.RetCode != 0 {
		return fmt.Errorf("CancelAllOrdersWs: code=%d msg=%s", resp.RetCode, resp.RetMsg)
	}
	return nil
}
