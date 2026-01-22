package bot

import (
	"context"
	"log"
	"time"

	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/store"
	"mm-platform-engine/internal/types"
)

// startUserStream subscribes to user stream events
func (b *Bot) startUserStream(ctx context.Context) error {
	log.Println("=== Starting User Stream Subscription ===")

	handlers := exchange.UserStreamHandlers{
		OnAccountUpdate: b.handleAccountUpdate,
		OnOrderUpdate:   b.handleOrderUpdate,
		OnFill:          b.handleFill,
		OnError:         b.handleStreamError,
	}

	log.Println("Connecting to user stream WebSocket...")
	if err := b.exchange.SubscribeUserStream(ctx, handlers); err != nil {
		log.Printf("ERROR: Failed to subscribe to user stream: %v", err)
		b.streamConnected = false
		return err
	}

	log.Println("✓ User stream WebSocket connected successfully")
	log.Println("✓ Listening for real-time events: account updates, order updates, fills")

	b.streamConnected = true
	b.lastMessageTime = time.Now()

	return nil
}

// handleAccountUpdate processes account balance updates
func (b *Bot) handleAccountUpdate(event *types.AccountEvent) {
	b.lastMessageTime = time.Now()

	log.Printf("📊 [USER STREAM] Account update: %d balances at %s",
		len(event.Balances), event.Timestamp.Format("15:04:05"))

	// Update cached balance state (used by getBalanceState)
	b.balanceMu.Lock()
	for i := range event.Balances {
		balance := &event.Balances[i]
		b.cachedBalance[balance.Asset] = balance
		log.Printf("   💰 %s: Free=%.8f Locked=%.8f", balance.Asset, balance.Free, balance.Locked)
	}
	b.balanceMu.Unlock()

	// Publish to Redis
	if err := b.redis.PublishAccountUpdate(b.ctx, event); err != nil {
		log.Printf("Failed to publish account update: %v", err)
	}

	// Update balance in Redis with separate free and locked amounts
	for _, balance := range event.Balances {
		if err := b.redis.SetBalance(b.ctx, b.cfg.TradingConfig.Symbol, balance.Asset, balance.Free, balance.Locked); err != nil {
			log.Printf("Failed to set balance for %s: %v", balance.Asset, err)
		}
	}
}

// handleOrderUpdate processes order state changes
func (b *Bot) handleOrderUpdate(event *types.OrderEvent) {
	b.lastMessageTime = time.Now()

	log.Printf("📝 [USER STREAM] Order update: %s %s %s @ %.8f (status: %s)",
		event.Symbol, event.Side, event.OrderID, event.Price, event.Status)

	// Save order to Redis (replaces manual save after order creation)
	orderInfo := &store.OrderInfo{
		OrderID:       event.OrderID,
		ClientOrderID: event.ClientOrderID,
		Symbol:        event.Symbol,
		Side:          event.Side,
		Price:         event.Price,
		Quantity:      event.Quantity,
		CreatedAt:     event.Timestamp.UnixMilli(),
		Status:        event.Status,
	}
	if err := b.redis.SaveOrder(b.ctx, orderInfo); err != nil {
		log.Printf("Failed to save order to Redis: %v", err)
	}

	// Publish to Redis
	if err := b.redis.PublishOrderUpdate(b.ctx, event); err != nil {
		log.Printf("Failed to publish order update: %v", err)
	}
}

// handleFill processes trade executions
func (b *Bot) handleFill(event *types.FillEvent) {
	b.lastMessageTime = time.Now()

	log.Printf("✅ [USER STREAM] FILL: %s %s %.8f @ %.8f (TradeID: %s, OrderID: %s)",
		event.Symbol, event.Side, event.Quantity, event.Price, event.TradeID, event.OrderID)

	// Record fill in metrics aggregator
	if b.metricsAgg != nil {
		side := types.OrderSideBuy
		if event.Side == "SELL" {
			side = types.OrderSideSell
		}
		fillTimestamp := event.Timestamp.UnixMilli()
		b.metricsAgg.RecordFill(side, event.Price, event.Quantity, fillTimestamp)

		// Calculate TTF (time-to-fill) if we have order creation time
		orderInfo, err := b.redis.GetOrder(b.ctx, event.Symbol, event.OrderID)
		if err == nil && orderInfo != nil && orderInfo.CreatedAt > 0 {
			ttfMs := fillTimestamp - orderInfo.CreatedAt
			ttfSec := float64(ttfMs) / 1000.0
			if ttfSec > 0 && ttfSec < 3600 { // Sanity check: TTF < 1 hour
				b.metricsAgg.RecordTimeToFill(ttfSec)
				log.Printf("   ⏱️  TTF: %.2f seconds", ttfSec)
			}
		}
	}

	// Publish to Redis
	if err := b.redis.PublishFill(b.ctx, event); err != nil {
		log.Printf("Failed to publish fill: %v", err)
	}

	// Save to MongoDB
	if err := b.mongo.SaveFill(b.ctx, event); err != nil {
		log.Printf("Failed to save fill: %v", err)
	}

	// Check for completed grid deals (matched buy-sell pairs)
	b.checkForCompletedDeals(event)
}

// handleStreamError processes WebSocket errors
func (b *Bot) handleStreamError(err error) {
	log.Printf("❌ [USER STREAM] WebSocket ERROR: %v", err)
	log.Println("⚠️  User stream connection lost - attempting to reconnect...")

	// Attempt to reconnect
	go b.reconnectUserStream()
}

// reconnectUserStream attempts to reconnect to the user stream
func (b *Bot) reconnectUserStream() {
	// Use mutex to ensure only one reconnection attempt at a time
	b.reconnectMu.Lock()
	if b.reconnecting {
		b.reconnectMu.Unlock()
		log.Println("⏭️  Reconnection already in progress, skipping")
		return
	}
	b.reconnecting = true
	b.reconnectMu.Unlock()

	defer func() {
		b.reconnectMu.Lock()
		b.reconnecting = false
		b.reconnectMu.Unlock()
	}()

	maxRetries := 10
	retryDelay := 2 * time.Second

	for i := 0; i < maxRetries; i++ {
		// Check if context is cancelled (bot is shutting down)
		select {
		case <-b.ctx.Done():
			log.Println("⚠️  Bot is shutting down, aborting reconnection")
			return
		default:
		}

		log.Printf("🔄 Reconnection attempt %d/%d...", i+1, maxRetries)

		// Wait before retrying (skip wait on first attempt)
		if i > 0 {
			time.Sleep(retryDelay)
		}

		// Try to restart user stream
		if err := b.startUserStream(b.ctx); err != nil {
			log.Printf("❌ Reconnection attempt %d failed: %v", i+1, err)
			// Exponential backoff up to 30 seconds
			retryDelay = time.Duration(float64(retryDelay) * 1.5)
			if retryDelay > 30*time.Second {
				retryDelay = 30 * time.Second
			}
			continue
		}

		log.Println("✅ User stream reconnected successfully!")
		b.streamConnected = true
		b.lastMessageTime = time.Now()
		return
	}

	log.Printf("❌ Failed to reconnect user stream after %d attempts", maxRetries)
	log.Println("⚠️  Bot will continue without real-time updates")
	b.streamConnected = false
}

// checkForCompletedDeals checks if a fill completes a grid deal
func (b *Bot) checkForCompletedDeals(fill *types.FillEvent) {
	// This is a simplified version. A real implementation would track
	// buy-sell pairs and calculate actual profit
	// For now, just log that we would check for deals
	log.Printf("Checking for completed deals after fill: %s", fill.TradeID)
}
