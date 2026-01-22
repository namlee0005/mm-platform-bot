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

// PublishFill publishes a fill event to Redis
func (s *RedisStore) PublishFill(ctx context.Context, fill *types.FillEvent) error {
	data, err := json.Marshal(fill)
	if err != nil {
		return fmt.Errorf("failed to marshal fill: %w", err)
	}

	channel := fmt.Sprintf("fills:%s", fill.Symbol)
	if err := s.client.Publish(ctx, channel, data).Err(); err != nil {
		return fmt.Errorf("failed to publish fill: %w", err)
	}

	return nil
}

// PublishAccountUpdate publishes an account update to Redis
func (s *RedisStore) PublishAccountUpdate(ctx context.Context, account *types.AccountEvent) error {
	data, err := json.Marshal(account)
	if err != nil {
		return fmt.Errorf("failed to marshal account: %w", err)
	}

	if err := s.client.Publish(ctx, "account:updates", data).Err(); err != nil {
		return fmt.Errorf("failed to publish account update: %w", err)
	}

	return nil
}

// PublishOrderUpdate publishes an order update to Redis
func (s *RedisStore) PublishOrderUpdate(ctx context.Context, order *types.OrderEvent) error {
	data, err := json.Marshal(order)
	if err != nil {
		return fmt.Errorf("failed to marshal order: %w", err)
	}

	channel := fmt.Sprintf("orders:%s", order.Symbol)
	if err := s.client.Publish(ctx, channel, data).Err(); err != nil {
		return fmt.Errorf("failed to publish order update: %w", err)
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
	Symbol        string  `json:"symbol"`
	Side          string  `json:"side"`
	Price         float64 `json:"price"`
	Quantity      float64 `json:"quantity"`
	CreatedAt     int64   `json:"createdAt"` // Unix milliseconds
	Status        string  `json:"status"`
}

// SaveOrder saves order information to Redis List
// Key format: order:{symbol}
func (s *RedisStore) SaveOrder(ctx context.Context, order *OrderInfo) error {
	key := fmt.Sprintf("order:%s", order.Symbol)

	data, err := json.Marshal(order)
	if err != nil {
		return fmt.Errorf("failed to marshal order: %w", err)
	}

	// Add to list (RPUSH)
	if err := s.client.RPush(ctx, key, data).Err(); err != nil {
		return fmt.Errorf("failed to save order to list: %w", err)
	}

	// Set expiration on the list key (24h)
	s.client.Expire(ctx, key, 24*time.Hour)

	return nil
}

// GetOrder retrieves order information from Redis List by orderId
func (s *RedisStore) GetOrder(ctx context.Context, symbol, orderID string) (*OrderInfo, error) {
	key := fmt.Sprintf("order:%s", symbol)

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
func (s *RedisStore) DeleteOrder(ctx context.Context, symbol, orderID string) error {
	key := fmt.Sprintf("order:%s", symbol)

	// Get all items to find the one to remove
	items, err := s.client.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return fmt.Errorf("failed to get orders from list: %w", err)
	}

	// Find and remove the order
	for _, item := range items {
		var order OrderInfo
		if err := json.Unmarshal([]byte(item), &order); err != nil {
			continue
		}
		if order.OrderID == orderID {
			// Remove this item from list (LREM removes by value)
			s.client.LRem(ctx, key, 1, item)
			break
		}
	}

	return nil
}

// GetAllOrders retrieves all orders for a symbol from Redis List
func (s *RedisStore) GetAllOrders(ctx context.Context, symbol string) ([]*OrderInfo, error) {
	key := fmt.Sprintf("order:%s", symbol)

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
func (s *RedisStore) ClearAllOrders(ctx context.Context, symbol string) error {
	key := fmt.Sprintf("order:%s", symbol)

	if err := s.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("failed to delete orders list: %w", err)
	}

	return nil
}
