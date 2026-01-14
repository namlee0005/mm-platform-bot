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

// SaveOrder saves order information to Redis
func (s *RedisStore) SaveOrder(ctx context.Context, order *OrderInfo) error {
	key := fmt.Sprintf("order:%s:%s", order.Symbol, order.OrderID)

	data, err := json.Marshal(order)
	if err != nil {
		return fmt.Errorf("failed to marshal order: %w", err)
	}

	// Save with 24h expiration (orders shouldn't live that long in MM)
	if err := s.client.Set(ctx, key, data, 24*time.Hour).Err(); err != nil {
		return fmt.Errorf("failed to save order: %w", err)
	}

	return nil
}

// GetOrder retrieves order information from Redis by orderId
func (s *RedisStore) GetOrder(ctx context.Context, symbol, orderID string) (*OrderInfo, error) {
	key := fmt.Sprintf("order:%s:%s", symbol, orderID)

	data, err := s.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, nil // Order not found
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get order: %w", err)
	}

	var order OrderInfo
	if err := json.Unmarshal(data, &order); err != nil {
		return nil, fmt.Errorf("failed to unmarshal order: %w", err)
	}

	return &order, nil
}

// DeleteOrder removes order from Redis by orderId
func (s *RedisStore) DeleteOrder(ctx context.Context, symbol, orderID string) error {
	key := fmt.Sprintf("order:%s:%s", symbol, orderID)

	if err := s.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("failed to delete order: %w", err)
	}

	return nil
}

// GetAllOrders retrieves all orders for a symbol
func (s *RedisStore) GetAllOrders(ctx context.Context, symbol string) ([]*OrderInfo, error) {
	pattern := fmt.Sprintf("order:%s:*", symbol)

	keys, err := s.client.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get order keys: %w", err)
	}

	orders := make([]*OrderInfo, 0, len(keys))
	for _, key := range keys {
		data, err := s.client.Get(ctx, key).Bytes()
		if err != nil {
			continue // Skip if error
		}

		var order OrderInfo
		if err := json.Unmarshal(data, &order); err != nil {
			continue // Skip if unmarshal error
		}

		orders = append(orders, &order)
	}

	return orders, nil
}

// ClearAllOrders deletes all orders for a symbol from Redis
func (s *RedisStore) ClearAllOrders(ctx context.Context, symbol string) error {
	pattern := fmt.Sprintf("order:%s:*", symbol)

	keys, err := s.client.Keys(ctx, pattern).Result()
	if err != nil {
		return fmt.Errorf("failed to get order keys: %w", err)
	}

	if len(keys) == 0 {
		return nil // No orders to clear
	}

	if err := s.client.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("failed to delete orders: %w", err)
	}

	return nil
}
