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

	"mm-platform-engine/internal/types"

	"github.com/gorilla/websocket"
)

// ────────────────────────────────────────────────────────────────────────────
// Types (wsPrivate prefix avoids conflicts with bybit_native_adapter.go)
// ────────────────────────────────────────────────────────────────────────────

// wsPrivateAuthResp is the auth-ack envelope from the Bybit V5 private WebSocket.
// The private WS uses {success, ret_msg} rather than {retCode, retMsg}.
type wsPrivateAuthResp struct {
	Success bool   `json:"success"`
	RetMsg  string `json:"ret_msg"`
	Op      string `json:"op"`
	ConnID  string `json:"conn_id"`
}

// ────────────────────────────────────────────────────────────────────────────
// BybitWSPrivate
// ────────────────────────────────────────────────────────────────────────────

// BybitWSPrivate is a standalone Bybit V5 private WebSocket client.
//
// It connects to wss://stream.bybit.com/v5/private, authenticates, subscribes
// to the order / execution / wallet topics, and delivers events to the
// registered UserStreamHandlers callbacks.
//
// Concurrency model:
//   - handlersMu: RWMutex guarding handlers — cheap reads, rare writes.
//   - connMu: serializes all writes to conn (gorilla write-serialization rule).
//   - conn field: always read/written under connMu.
//   - readLoop / keepalive: both receive a conn snapshot and exit when that
//     snapshot is no longer p.conn (keepalive checks under connMu; readLoop
//     exits implicitly when its conn closes).
//   - orderStateMu: guards dedup caches (orderStateCache, seenTradeIDs).
type BybitWSPrivate struct {
	apiKey    string
	apiSecret string
	url       string

	conn   *websocket.Conn
	connMu sync.Mutex // serializes all writes to conn

	handlers   UserStreamHandlers
	handlersMu sync.RWMutex

	// Order state dedup — prevents duplicate events on WS reconnect snapshots.
	// Shares bybitNativeOrderState type with bybit_native_adapter.go (same pkg).
	orderStateMu    sync.Mutex
	orderStateCache map[string]*bybitNativeOrderState
	seenTradeIDs    map[string]bool
	lastGCTime      time.Time

	ctx    context.Context
	cancel context.CancelFunc

	connected atomic.Bool
}

// NewBybitWSPrivate creates a private WS client. Call Connect before use.
func NewBybitWSPrivate(apiKey, apiSecret string, sandbox bool) *BybitWSPrivate {
	u := bybitWSPrivateURL
	if sandbox {
		u = bybitTestWSPrivateURL
	}
	return &BybitWSPrivate{
		apiKey:          apiKey,
		apiSecret:       apiSecret,
		url:             u,
		orderStateCache: make(map[string]*bybitNativeOrderState),
		seenTradeIDs:    make(map[string]bool),
	}
}

// SetHandlers registers the user stream callbacks.
// Safe to call before or after Connect — handlers are swapped atomically.
func (p *BybitWSPrivate) SetHandlers(h UserStreamHandlers) {
	p.handlersMu.Lock()
	p.handlers = h
	p.handlersMu.Unlock()
}

// ════════════════════════════════════════════════════════════════════════════
// Lifecycle
// ════════════════════════════════════════════════════════════════════════════

// Connect dials the private WebSocket, authenticates, subscribes to topics, and
// starts background read and keepalive goroutines. The provided ctx controls all
// background workers — cancel it to shut down cleanly.
func (p *BybitWSPrivate) Connect(ctx context.Context) error {
	p.ctx, p.cancel = context.WithCancel(ctx)
	return p.dialAndStart()
}

// Close shuts down the connection and all background goroutines.
func (p *BybitWSPrivate) Close() error {
	if p.cancel != nil {
		p.cancel()
	}
	p.connected.Store(false)

	p.connMu.Lock()
	conn := p.conn
	p.connMu.Unlock()

	if conn != nil {
		p.connMu.Lock()
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		p.connMu.Unlock()
		conn.Close()
	}

	log.Printf("[BYBIT-WS-PRIVATE] Closed")
	return nil
}

// IsConnected returns true when the WS is live and subscribed.
func (p *BybitWSPrivate) IsConnected() bool { return p.connected.Load() }

// dialAndStart dials, authenticates, subscribes, and launches goroutines.
// Safe to call from reconnect without touching p.ctx / p.cancel.
func (p *BybitWSPrivate) dialAndStart() error {
	conn, _, err := websocket.DefaultDialer.Dial(p.url, nil)
	if err != nil {
		return fmt.Errorf("bybit private WS dial: %w", err)
	}

	// Authenticate synchronously — read loop is not running yet.
	if err := p.authenticate(conn); err != nil {
		conn.Close()
		return fmt.Errorf("bybit private WS auth: %w", err)
	}

	// Subscribe to order/execution/wallet topics.
	if err := p.subscribe(conn); err != nil {
		conn.Close()
		return fmt.Errorf("bybit private WS subscribe: %w", err)
	}

	// Replace the active connection under connMu so keepalive can detect replacement.
	p.connMu.Lock()
	p.conn = conn
	p.connMu.Unlock()

	p.connected.Store(true)
	go p.readLoop(conn)
	go p.keepalive(conn)

	log.Printf("[BYBIT-WS-PRIVATE] Connected and subscribed (%s)", p.url)
	return nil
}

// ════════════════════════════════════════════════════════════════════════════
// Authentication & subscription
// ════════════════════════════════════════════════════════════════════════════

// authenticate sends HMAC-SHA256 auth and reads the ack synchronously.
// Private WS auth response format: {"success": true, "ret_msg": "", "op": "auth"}
// Signature: HMAC_SHA256(secret, "GET/realtime" + expires_ms)
func (p *BybitWSPrivate) authenticate(conn *websocket.Conn) error {
	expires := time.Now().UnixMilli() + 10000 // 10-second window
	signStr := "GET/realtime" + strconv.FormatInt(expires, 10)
	mac := hmac.New(sha256.New, []byte(p.apiSecret))
	mac.Write([]byte(signStr))
	sig := hex.EncodeToString(mac.Sum(nil))

	authMsg := map[string]interface{}{
		"reqId": "auth",
		"op":    "auth",
		"args":  []interface{}{p.apiKey, expires, sig},
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

	var resp wsPrivateAuthResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("parse auth ack: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("auth rejected: %s", resp.RetMsg)
	}
	return nil
}

// subscribe sends the subscription request for order / execution / wallet topics.
func (p *BybitWSPrivate) subscribe(conn *websocket.Conn) error {
	subMsg := map[string]interface{}{
		"op":   "subscribe",
		"args": []string{"order", "execution", "wallet"},
	}
	conn.SetWriteDeadline(time.Now().Add(bybitWSTimeout))
	if err := conn.WriteJSON(subMsg); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	return nil
}

// ════════════════════════════════════════════════════════════════════════════
// Background goroutines
// ════════════════════════════════════════════════════════════════════════════

// readLoop reads messages from conn and routes them to handlers.
// Takes a conn snapshot: exits when that specific connection dies, not when
// p.conn is replaced (reconnect launches a fresh readLoop for the new conn).
func (p *BybitWSPrivate) readLoop(conn *websocket.Conn) {
	defer log.Printf("[BYBIT-WS-PRIVATE] Read loop exited")

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if p.ctx.Err() != nil {
				return // normal shutdown via Close()
			}
			log.Printf("[BYBIT-WS-PRIVATE] Read error: %v — reconnecting", err)
			p.connected.Store(false)

			p.handlersMu.RLock()
			onErr := p.handlers.OnError
			p.handlersMu.RUnlock()
			if onErr != nil {
				onErr(fmt.Errorf("private WS disconnected: %w", err))
			}

			go p.reconnect()
			return
		}
		p.handleMessage(raw)
	}
}

// keepalive sends JSON pings every bybitPingInterval.
// Scoped to conn snapshot — exits automatically when that conn is replaced.
func (p *BybitWSPrivate) keepalive(conn *websocket.Conn) {
	ticker := time.NewTicker(bybitPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.connMu.Lock()
			stale := p.conn != conn // conn was replaced by reconnect
			if !stale {
				conn.SetWriteDeadline(time.Now().Add(bybitWSTimeout))
			}
			var err error
			if !stale {
				err = conn.WriteJSON(map[string]string{"op": "ping"})
			}
			p.connMu.Unlock()

			if stale {
				return // this conn is no longer active
			}
			if err != nil {
				log.Printf("[BYBIT-WS-PRIVATE] Ping failed: %v", err)
				return
			}
		}
	}
}

// reconnect re-dials with exponential backoff until success or context cancellation.
func (p *BybitWSPrivate) reconnect() {
	for attempt := 1; ; attempt++ {
		if p.ctx.Err() != nil {
			return
		}
		delay := time.Duration(attempt) * 2 * time.Second
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		log.Printf("[BYBIT-WS-PRIVATE] Reconnecting in %v (attempt %d)...", delay, attempt)
		time.Sleep(delay)
		if p.ctx.Err() != nil {
			return
		}
		if err := p.dialAndStart(); err != nil {
			log.Printf("[BYBIT-WS-PRIVATE] Reconnect failed: %v", err)
			continue
		}
		log.Printf("[BYBIT-WS-PRIVATE] Reconnected after %d attempt(s)", attempt)
		return
	}
}

// ════════════════════════════════════════════════════════════════════════════
// Message dispatch
// ════════════════════════════════════════════════════════════════════════════

// handleMessage routes a raw WS message to the appropriate topic handler.
// Reuses bybitWSPrivateMsg from bybit_native_adapter.go (same package).
func (p *BybitWSPrivate) handleMessage(msg []byte) {
	var base bybitWSPrivateMsg
	if err := json.Unmarshal(msg, &base); err != nil {
		return
	}

	// Skip system messages
	if base.Op == "pong" || base.Op == "subscribe" || base.Op == "auth" {
		return
	}

	switch base.Topic {
	case "order":
		p.handleOrderUpdate(msg)
	case "execution":
		p.handleExecutionUpdate(msg)
	case "wallet":
		p.handleWalletUpdate(msg)
	}
}

// handleOrderUpdate processes order status change events.
// Deduplicates against the order state cache to prevent double-firing on reconnect.
func (p *BybitWSPrivate) handleOrderUpdate(msg []byte) {
	p.handlersMu.RLock()
	handler := p.handlers.OnOrderUpdate
	p.handlersMu.RUnlock()
	if handler == nil {
		return
	}

	var update bybitWSOrderUpdate
	if err := json.Unmarshal(msg, &update); err != nil {
		log.Printf("[BYBIT-WS-PRIVATE] unmarshal order update: %v", err)
		return
	}

	for _, d := range update.Data {
		if d.Category != bybitCategory {
			continue
		}

		status := bybitNativeMapOrderStatus(d.OrderStatus)
		filledQty := bybitNativeParseFloat(d.CumExecQty)

		if !p.updateOrderState(d.OrderID, status, filledQty) {
			continue // state unchanged — duplicate event
		}

		handler(&types.OrderEvent{
			OrderID:            d.OrderID,
			ClientOrderID:      d.OrderLinkID,
			Symbol:             d.Symbol,
			Side:               bybitNativeMapSide(d.Side),
			Type:               bybitNativeMapOrderType(d.OrderType),
			Status:             status,
			Price:              bybitNativeParseFloat(d.Price),
			Quantity:           bybitNativeParseFloat(d.Qty),
			ExecutedQty:        filledQty,
			CumulativeQuoteQty: bybitNativeParseFloat(d.CumExecValue),
			Timestamp:          bybitNativeParseTimestamp(d.UpdatedTime),
		})
	}
}

// handleExecutionUpdate processes trade fill events.
// Deduplicates by ExecID to prevent double-counting fees/fills on reconnect.
func (p *BybitWSPrivate) handleExecutionUpdate(msg []byte) {
	p.handlersMu.RLock()
	handler := p.handlers.OnFill
	p.handlersMu.RUnlock()
	if handler == nil {
		return
	}

	var update bybitWSExecutionUpdate
	if err := json.Unmarshal(msg, &update); err != nil {
		log.Printf("[BYBIT-WS-PRIVATE] unmarshal execution update: %v", err)
		return
	}

	for _, d := range update.Data {
		if d.Category != bybitCategory {
			continue
		}

		// Dedup by trade ID — seenTradeIDs is GC'd alongside orderStateCache.
		p.orderStateMu.Lock()
		if p.seenTradeIDs[d.ExecID] {
			p.orderStateMu.Unlock()
			continue
		}
		p.seenTradeIDs[d.ExecID] = true
		p.orderStateMu.Unlock()

		handler(&types.FillEvent{
			OrderID:         d.OrderID,
			ClientOrderID:   d.OrderLinkID,
			Symbol:          d.Symbol,
			Side:            bybitNativeMapSide(d.Side),
			Price:           bybitNativeParseFloat(d.ExecPrice),
			Quantity:        bybitNativeParseFloat(d.ExecQty),
			Commission:      bybitNativeParseFloat(d.ExecFee),
			CommissionAsset: d.FeeCurrency,
			TradeID:         d.ExecID,
			Timestamp:       bybitNativeParseTimestamp(d.ExecTime),
		})
	}
}

// handleWalletUpdate processes balance change events.
func (p *BybitWSPrivate) handleWalletUpdate(msg []byte) {
	p.handlersMu.RLock()
	handler := p.handlers.OnAccountUpdate
	p.handlersMu.RUnlock()
	if handler == nil {
		return
	}

	var update bybitWSWalletUpdate
	if err := json.Unmarshal(msg, &update); err != nil {
		log.Printf("[BYBIT-WS-PRIVATE] unmarshal wallet update: %v", err)
		return
	}

	for _, d := range update.Data {
		var balances []types.Balance
		for _, coin := range d.Coin {
			free := bybitNativeParseFloat(coin.Free)
			locked := bybitNativeParseFloat(coin.Locked)
			walletBal := bybitNativeParseFloat(coin.WalletBalance)
			// Bybit UNIFIED account may omit Free; derive it from WalletBalance.
			if free == 0 && walletBal > 0 {
				free = walletBal - locked
			}
			balances = append(balances, types.Balance{
				Asset:  coin.Coin,
				Free:   free,
				Locked: locked,
			})
		}
		if len(balances) > 0 {
			handler(&types.AccountEvent{
				Balances:  balances,
				Timestamp: time.Now(),
			})
		}
	}
}

// ════════════════════════════════════════════════════════════════════════════
// Order state dedup
// ════════════════════════════════════════════════════════════════════════════

// updateOrderState returns true if the order state changed (event should be emitted).
// Also GCs terminal entries every 60s to prevent unbounded memory growth.
func (p *BybitWSPrivate) updateOrderState(orderID, status string, filledQty float64) bool {
	p.orderStateMu.Lock()
	defer p.orderStateMu.Unlock()

	now := time.Now()
	if now.Sub(p.lastGCTime) > 60*time.Second {
		cutoff := now.Add(-5 * time.Minute)
		for id, state := range p.orderStateCache {
			if state.terminal && state.seenAt.Before(cutoff) {
				delete(p.orderStateCache, id)
			}
		}
		if len(p.seenTradeIDs) > 10000 {
			p.seenTradeIDs = make(map[string]bool)
		}
		p.lastGCTime = now
	}

	terminal := status == "FILLED" || status == "CANCELED" || status == "EXPIRED" || status == "REJECTED"
	prev, exists := p.orderStateCache[orderID]
	if exists && prev.status == status && prev.filledQty == filledQty {
		return false // unchanged
	}

	p.orderStateCache[orderID] = &bybitNativeOrderState{
		status:    status,
		filledQty: filledQty,
		seenAt:    now,
		terminal:  terminal,
	}
	return true
}
