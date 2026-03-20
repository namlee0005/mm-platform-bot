package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"mm-platform-engine/internal/types"

	"github.com/redis/go-redis/v9"
)

// RedisStore handles publishing events to Redis
type RedisStore struct {
	client *redis.Client
}

// NewRedisStore creates a new Redis store
func NewRedisStore(addr, password string, db int) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	// Test connection
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to redis: %w", err)
	}

	return &RedisStore{client: client}, nil
}

// PublishFill publishes a fill event to Redis Stream
func (s *RedisStore) PublishFill(ctx context.Context, fill *types.FillEvent) error {
	streamKey := fmt.Sprintf("fills:stream:%s", fill.Symbol)

	_, err := s.client.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey,
		MaxLen: 1000,
		Approx: true,
		Values: map[string]interface{}{
			"symbol":           fill.Symbol,
			"order_id":         fill.OrderID,
			"trade_id":         fill.TradeID,
			"side":             fill.Side,
			"price":            fill.Price,
			"quantity":         fill.Quantity,
			"commission":       fill.Commission,
			"commission_asset": fill.CommissionAsset,
			"timestamp":        fill.Timestamp.UnixMilli(),
		},
	}).Result()

	if err != nil {
		return fmt.Errorf("failed to add fill to stream: %w", err)
	}

	return nil
}

// PublishAccountUpdate publishes an account update to Redis Stream
func (s *RedisStore) PublishAccountUpdate(ctx context.Context, account *types.AccountEvent) error {
	// Marshal balances to JSON for storage in stream
	balancesJSON, err := json.Marshal(account.Balances)
	if err != nil {
		return fmt.Errorf("failed to marshal balances: %w", err)
	}

	_, err = s.client.XAdd(ctx, &redis.XAddArgs{
		Stream: "account:stream",
		MaxLen: 500,
		Approx: true,
		Values: map[string]interface{}{
			"balances":  string(balancesJSON),
			"timestamp": account.Timestamp.UnixMilli(),
		},
	}).Result()

	if err != nil {
		return fmt.Errorf("failed to add account update to stream: %w", err)
	}

	return nil
}

// PublishOrderUpdate publishes an order update to Redis Stream
func (s *RedisStore) PublishOrderUpdate(ctx context.Context, order *types.OrderEvent) error {
	streamKey := fmt.Sprintf("orders:stream:%s", order.Symbol)

	_, err := s.client.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey,
		MaxLen: 1000,
		Approx: true,
		Values: map[string]interface{}{
			"order_id":        order.OrderID,
			"client_order_id": order.ClientOrderID,
			"symbol":          order.Symbol,
			"side":            order.Side,
			"status":          order.Status,
			"price":           order.Price,
			"quantity":        order.Quantity,
			"executed_qty":    order.ExecutedQty,
			"timestamp":       order.Timestamp.UnixMilli(),
		},
	}).Result()

	if err != nil {
		return fmt.Errorf("failed to add order update to stream: %w", err)
	}

	return nil
}

// SetInventory stores current inventory (balances) in Redis
func (s *RedisStore) SetInventory(ctx context.Context, symbol, asset string, quantity float64) error {
	key := fmt.Sprintf("inventory:%s:%s", symbol, asset)
	if err := s.client.Set(ctx, key, quantity, 0).Err(); err != nil {
		return fmt.Errorf("failed to set inventory: %w", err)
	}
	return nil
}

// GetInventory retrieves inventory from Redis
func (s *RedisStore) GetInventory(ctx context.Context, symbol, asset string) (float64, error) {
	key := fmt.Sprintf("inventory:%s:%s", symbol, asset)
	val, err := s.client.Get(ctx, key).Float64()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to get inventory: %w", err)
	}
	return val, nil
}

// SetBalance stores both free and locked balances in Redis using hash
func (s *RedisStore) SetBalance(ctx context.Context, symbol, asset string, free, locked float64) error {
	key := fmt.Sprintf("balance:%s:%s", symbol, asset)

	// Store as hash with free and locked fields
	if err := s.client.HSet(ctx, key, map[string]interface{}{
		"free":       free,
		"locked":     locked,
		"total":      free + locked,
		"updated_at": time.Now().Unix(),
	}).Err(); err != nil {
		return fmt.Errorf("failed to set balance: %w", err)
	}

	return nil
}

// GetBalance retrieves free and locked balances from Redis
func (s *RedisStore) GetBalance(ctx context.Context, symbol, asset string) (free, locked float64, err error) {
	key := fmt.Sprintf("balance:%s:%s", symbol, asset)

	result, err := s.client.HGetAll(ctx, key).Result()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get balance: %w", err)
	}

	if len(result) == 0 {
		return 0, 0, nil // No data found
	}

	// Parse free and locked from hash
	if freeStr, ok := result["free"]; ok {
		fmt.Sscanf(freeStr, "%f", &free)
	}
	if lockedStr, ok := result["locked"]; ok {
		fmt.Sscanf(lockedStr, "%f", &locked)
	}

	return free, locked, nil
}

// MMBalance represents balance info for MM bot
type MMBalance struct {
	Free      float64 `json:"free"`
	Locked    float64 `json:"locked"`
	Total     float64 `json:"total"`
	UpdatedAt int64   `json:"updated_at"`
}

// SetMMBalance stores MM bot balance in Redis
// Key: balance:{exchange}:{symbol}, Field: {botId}, Value: JSON {free, locked, total, updated_at}
func (s *RedisStore) SetMMBalance(ctx context.Context, exchange, symbol, botID string, free, locked float64) error {
	key := fmt.Sprintf("balance:%s:%s", exchange, symbol)

	balanceData := MMBalance{
		Free:      free,
		Locked:    locked,
		Total:     free + locked,
		UpdatedAt: time.Now().Unix(),
	}

	jsonData, err := json.Marshal(balanceData)
	if err != nil {
		return fmt.Errorf("failed to marshal balance: %w", err)
	}

	if err := s.client.HSet(ctx, key, botID, string(jsonData)).Err(); err != nil {
		return fmt.Errorf("failed to set MM balance: %w", err)
	}

	return nil
}

// GetMMBalance retrieves MM bot balance from Redis
// Key: balance:{exchange}:{symbol}, Field: {botId}
func (s *RedisStore) GetMMBalance(ctx context.Context, exchange, symbol, botID string) (*MMBalance, error) {
	key := fmt.Sprintf("balance:%s:%s", exchange, symbol)

	jsonData, err := s.client.HGet(ctx, key, botID).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil // No balance found
		}
		return nil, fmt.Errorf("failed to get MM balance: %w", err)
	}

	var balance MMBalance
	if err := json.Unmarshal([]byte(jsonData), &balance); err != nil {
		return nil, fmt.Errorf("failed to unmarshal balance: %w", err)
	}

	return &balance, nil
}

// AssetBalance represents balance for a single asset
type AssetBalance struct {
	Asset  string  `json:"asset"`
	Free   float64 `json:"free"`
	Locked float64 `json:"locked"`
}

// BotBalances represents all asset balances for a bot
type BotBalances struct {
	Balances  map[string]MMBalance `json:"balances"` // asset -> balance
	UpdatedAt int64                `json:"updated_at"`
}

// SetMMBalances stores all asset balances for a bot in Redis
// Key: balance:{exchange}:{symbol}, Field: {botId}, Value: JSON {balances: {asset: {free, locked, total}}, updated_at}
func (s *RedisStore) SetMMBalances(ctx context.Context, exchange, symbol, botID string, balances []AssetBalance) error {
	key := fmt.Sprintf("balance:%s:%s", exchange, symbol)
	now := time.Now().Unix()

	// Build balances map
	balanceMap := make(map[string]MMBalance)
	for _, b := range balances {
		// Skip zero balances
		if b.Free == 0 && b.Locked == 0 {
			continue
		}
		balanceMap[b.Asset] = MMBalance{
			Free:      b.Free,
			Locked:    b.Locked,
			Total:     b.Free + b.Locked,
			UpdatedAt: now,
		}
	}

	botBalances := BotBalances{
		Balances:  balanceMap,
		UpdatedAt: now,
	}

	jsonData, err := json.Marshal(botBalances)
	if err != nil {
		return fmt.Errorf("failed to marshal balances: %w", err)
	}

	if err := s.client.HSet(ctx, key, botID, string(jsonData)).Err(); err != nil {
		return fmt.Errorf("failed to set MM balances: %w", err)
	}

	return nil
}

// ClearMMBalances removes balance data for a specific bot from Redis
// Key: balance:{exchange}:{symbol}, Field: {botId}
func (s *RedisStore) ClearMMBalances(ctx context.Context, exchange, symbol, botID string) error {
	key := fmt.Sprintf("balance:%s:%s", exchange, symbol)

	if err := s.client.HDel(ctx, key, botID).Err(); err != nil {
		return fmt.Errorf("failed to clear MM balances: %w", err)
	}

	return nil
}

// GetMMBalances retrieves all bot balances for a symbol from Redis
// Key: balance:{exchange}:{symbol}
func (s *RedisStore) GetMMBalances(ctx context.Context, exchange, symbol string) (map[string]*BotBalances, error) {
	key := fmt.Sprintf("balance:%s:%s", exchange, symbol)

	result, err := s.client.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get MM balances: %w", err)
	}

	allBalances := make(map[string]*BotBalances)
	for botID, jsonData := range result {
		var botBalances BotBalances
		if err := json.Unmarshal([]byte(jsonData), &botBalances); err != nil {
			continue
		}
		allBalances[botID] = &botBalances
	}

	return allBalances, nil
}

// GetBotBalances retrieves balances for a specific bot
// Key: balance:{exchange}:{symbol}, Field: {botId}
func (s *RedisStore) GetBotBalances(ctx context.Context, exchange, symbol, botID string) (*BotBalances, error) {
	key := fmt.Sprintf("balance:%s:%s", exchange, symbol)

	jsonData, err := s.client.HGet(ctx, key, botID).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get bot balances: %w", err)
	}

	var botBalances BotBalances
	if err := json.Unmarshal([]byte(jsonData), &botBalances); err != nil {
		return nil, fmt.Errorf("failed to unmarshal balances: %w", err)
	}

	return &botBalances, nil
}

// SetStatus stores bot status in Redis
func (s *RedisStore) SetStatus(ctx context.Context, symbol, status string) error {
	key := fmt.Sprintf("status:%s", symbol)
	if err := s.client.Set(ctx, key, status, 0).Err(); err != nil {
		return fmt.Errorf("failed to set status: %w", err)
	}
	return nil
}

// Close closes the Redis connection
func (s *RedisStore) Close() error {
	return s.client.Close()
}

// OrderInfo represents order information stored in Redis
type OrderInfo struct {
	OrderID       string  `json:"orderId"`
	ClientOrderID string  `json:"clientOrderId"`
	Exchange      string  `json:"exchange"`
	Symbol        string  `json:"symbol"`
	Side          string  `json:"side"`
	Price         float64 `json:"price"`
	Quantity      float64 `json:"quantity"`
	CreatedAt     int64   `json:"createdAt"` // Unix milliseconds
	Status        string  `json:"status"`
	BotID         string  `json:"botId,omitempty"` // Bot instance ID
}

// SaveOrder saves order information to Redis List
// Key format: order:{exchange}:{symbol}
func (s *RedisStore) SaveOrder(ctx context.Context, order *OrderInfo) error {
	key := fmt.Sprintf("order:%s:%s", order.Exchange, order.Symbol)

	data, err := json.Marshal(order)
	if err != nil {
		return fmt.Errorf("failed to marshal order: %w", err)
	}

	// Add to list (RPUSH)
	if err := s.client.RPush(ctx, key, data).Err(); err != nil {
		return fmt.Errorf("failed to save order to list: %w", err)
	}

	// No expiration - orders should persist until explicitly deleted
	// Active orders need to be visible to new users/clients

	return nil
}

// GetOrder retrieves order information from Redis List by orderId
func (s *RedisStore) GetOrder(ctx context.Context, exchange, symbol, orderID string) (*OrderInfo, error) {
	key := fmt.Sprintf("order:%s:%s", exchange, symbol)

	// Get all items from list
	items, err := s.client.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get orders from list: %w", err)
	}

	// Find the order by orderId
	for _, item := range items {
		var order OrderInfo
		if err := json.Unmarshal([]byte(item), &order); err != nil {
			continue
		}
		if order.OrderID == orderID {
			return &order, nil
		}
	}

	return nil, nil // Order not found
}

// DeleteOrder removes order from Redis List by orderId
func (s *RedisStore) DeleteOrder(ctx context.Context, exchange, symbol, orderID string) error {
	key := fmt.Sprintf("order:%s:%s", exchange, symbol)

	// Get all items to find the one to remove
	items, err := s.client.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return fmt.Errorf("failed to get orders from list: %w", err)
	}

	// Find and remove the order
	found := false
	for _, item := range items {
		var order OrderInfo
		if err := json.Unmarshal([]byte(item), &order); err != nil {
			continue
		}
		if order.OrderID == orderID {
			// Remove this item from list (LREM removes by value)
			removed, err := s.client.LRem(ctx, key, 1, item).Result()
			if err != nil {
				return fmt.Errorf("failed to remove order from list: %w", err)
			}
			if removed > 0 {
				found = true
			}
			break
		}
	}

	if !found {
		return fmt.Errorf("order %s not found in list", orderID)
	}

	return nil
}

// GetAllOrders retrieves all orders for a symbol from Redis List
func (s *RedisStore) GetAllOrders(ctx context.Context, exchange, symbol string) ([]*OrderInfo, error) {
	key := fmt.Sprintf("order:%s:%s", exchange, symbol)

	items, err := s.client.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get orders from list: %w", err)
	}

	orders := make([]*OrderInfo, 0, len(items))
	for _, item := range items {
		var order OrderInfo
		if err := json.Unmarshal([]byte(item), &order); err != nil {
			continue // Skip if unmarshal error
		}
		orders = append(orders, &order)
	}

	return orders, nil
}

// ClearAllOrders deletes all orders for a symbol from Redis (deletes the list)
func (s *RedisStore) ClearAllOrders(ctx context.Context, exchange, symbol string) error {
	key := fmt.Sprintf("order:%s:%s", exchange, symbol)

	if err := s.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("failed to delete orders list: %w", err)
	}

	return nil
}

// ClearOrdersByBotID removes only orders belonging to a specific bot
// This is safer than ClearAllOrders when multiple bots share the same symbol
func (s *RedisStore) ClearOrdersByBotID(ctx context.Context, exchange, symbol, botID string) (int, error) {
	key := fmt.Sprintf("order:%s:%s", exchange, symbol)

	// Get all items from list
	items, err := s.client.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return 0, fmt.Errorf("failed to get orders from list: %w", err)
	}

	// Find and remove orders belonging to this bot
	removedCount := 0
	for _, item := range items {
		var order OrderInfo
		if err := json.Unmarshal([]byte(item), &order); err != nil {
			continue
		}
		if order.BotID == botID {
			removed, err := s.client.LRem(ctx, key, 1, item).Result()
			if err != nil {
				continue // Log but don't fail
			}
			if removed > 0 {
				removedCount++
			}
		}
	}

	return removedCount, nil
}

// MMOrderEvent represents an MM engine order event for FE
type MMOrderEvent struct {
	Type      string  `json:"type"` // "place", "cancel", "amend", "fill"
	Exchange  string  `json:"exchange"`
	Symbol    string  `json:"symbol"`
	OrderID   string  `json:"order_id"`
	Side      string  `json:"side"` // "BUY" or "SELL"
	Price     float64 `json:"price"`
	Qty       float64 `json:"qty"`
	Level     int     `json:"level"`     // ladder level index
	Reason    string  `json:"reason"`    // why this action was taken
	Timestamp int64   `json:"timestamp"` // unix milliseconds
	BotID     string  `json:"bot_id"`    // unique bot instance ID
}

// PublishMMOrderEvent publishes MM order event to Redis Stream
// Stream key format: mm:stream:{exchange}:{symbol}
// Uses XADD with MAXLEN ~1000 to limit memory usage
func (s *RedisStore) PublishMMOrderEvent(ctx context.Context, event *MMOrderEvent) error {
	streamKey := fmt.Sprintf("mm:stream:%s:%s", event.Exchange, event.Symbol)

	// Add to stream with auto-generated ID and approximate maxlen
	_, err := s.client.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey,
		MaxLen: 1000, // Keep last ~1000 messages
		Approx: true, // Use ~ for better performance
		Values: map[string]interface{}{
			"type":      event.Type,
			"exchange":  event.Exchange,
			"symbol":    event.Symbol,
			"order_id":  event.OrderID,
			"side":      event.Side,
			"price":     event.Price,
			"qty":       event.Qty,
			"level":     event.Level,
			"reason":    event.Reason,
			"timestamp": event.Timestamp,
			"bot_id":    event.BotID,
		},
	}).Result()

	if err != nil {
		return fmt.Errorf("failed to add MM order event to stream: %w", err)
	}

	return nil
}
