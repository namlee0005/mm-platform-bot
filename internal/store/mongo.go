package store

import (
	"context"
	"fmt"
	"log"
	"time"

	"mm-platform-engine/internal/types"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	CollectionOrderHistory = "order_history"
	CollectionDeals        = "deals"
)

// MongoStore handles persisting data to MongoDB
type MongoStore struct {
	client   *mongo.Client
	database *mongo.Database
}

// NewMongoStore creates a new MongoDB store
func NewMongoStore(uri, database string) (*MongoStore, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

		opts := options.Client().ApplyURI(uri).SetServerSelectionTimeout(5 * time.Second)
	client, err := mongo.Connect(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to mongodb: %w", err)
	}

	// Ping to verify connection
	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("failed to ping mongodb: %w", err)
	}

	db := client.Database(database)

	return &MongoStore{
		client:   client,
		database: db,
	}, nil
}

// SaveDeal saves a completed deal to MongoDB
func (s *MongoStore) SaveDeal(ctx context.Context, deal *types.DealEvent) error {
	collection := s.database.Collection(CollectionDeals)
	_, err := collection.InsertOne(ctx, deal)
	if err != nil {
		return fmt.Errorf("failed to save deal: %w", err)
	}
	return nil
}

// InsertFilledOrder inserts a filled order into order_history if not already exists (deduped by tradeId in clientOrderId).
func (s *MongoStore) InsertFilledOrder(ctx context.Context, order *types.OrderEvent) error {
	collection := s.database.Collection(CollectionOrderHistory)
	filter := bson.M{"clientOrderId": order.ClientOrderID}
	update := bson.M{"$setOnInsert": order}
	opts := options.Update().SetUpsert(true)
	_, err := collection.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return fmt.Errorf("failed to insert filled order: %w", err)
	}
	return nil
}

// GetLatestOrderTimestamp returns the timestamp of the most recent order in the orders collection.
// Used to determine the `since` parameter for FetchClosedOrders.
func (s *MongoStore) GetLatestOrderTimestamp(ctx context.Context, exchange, symbol, botID string) (time.Time, error) {
	collection := s.database.Collection(CollectionOrderHistory)
	filter := bson.M{}
	if exchange != "" {
		filter["exchange"] = exchange
	}
	if symbol != "" {
		filter["symbol"] = symbol
	}
	if botID != "" {
		filter["botId"] = botID
	}
	opts := options.FindOne().SetSort(bson.D{{Key: "timestamp", Value: -1}})

	var result struct {
		Timestamp time.Time `bson:"timestamp"`
	}
	err := collection.FindOne(ctx, filter, opts).Decode(&result)
	if err != nil {
		return time.Time{}, err
	}
	return result.Timestamp, nil
}

// GetFillsInWindow retrieves filled orders within a time window (from sinceTime to now)
func (s *MongoStore) GetFillsInWindow(ctx context.Context, symbol string, sinceTime time.Time) ([]*types.OrderEvent, error) {
	collection := s.database.Collection(CollectionOrderHistory)
	filter := bson.M{
		"symbol": symbol,
		"timestamp": bson.M{
			"$gte": sinceTime,
		},
	}
	opts := options.Find().SetSort(bson.D{{Key: "timestamp", Value: 1}}) // Oldest first

	cursor, err := collection.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to query fills in window: %w", err)
	}
	defer cursor.Close(ctx)

	var orders []*types.OrderEvent
	if err := cursor.All(ctx, &orders); err != nil {
		return nil, fmt.Errorf("failed to decode orders: %w", err)
	}

	return orders, nil
}

// Close closes the MongoDB connection
func (s *MongoStore) Close(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}

// ConfigUpdate represents an updated simple_config from MongoDB
type ConfigUpdate struct {
	IsUpdated    bool
	SimpleConfig *types.SimpleConfigUpdate
}

// CheckConfigUpdate checks if config has been updated and returns new simple_config if so
func (s *MongoStore) CheckConfigUpdate(ctx context.Context, keyID string) (*ConfigUpdate, error) {
	collection := s.database.Collection("user_exchange_keys")

	// Parse ObjectID
	objectID, err := primitive.ObjectIDFromHex(keyID)
	if err != nil {
		return nil, fmt.Errorf("invalid ObjectID: %w", err)
	}

	// Find the document and check isConfigUpdated
	var result struct {
		IsConfigUpdated *bool                    `bson:"isConfigUpdated"`
		SimpleConfig    types.SimpleConfigUpdate `bson:"config"`
	}

	err = collection.FindOne(ctx, bson.M{"_id": objectID}).Decode(&result)
	if err != nil {
		return nil, fmt.Errorf("failed to find user exchange key: %w", err)
	}

	// Check if isConfigUpdated is true
	if result.IsConfigUpdated == nil || !*result.IsConfigUpdated {
		return &ConfigUpdate{IsUpdated: false}, nil
	}

	// Reset isConfigUpdated to false
	_, err = collection.UpdateOne(ctx,
		bson.M{"_id": objectID},
		bson.M{"$set": bson.M{"isConfigUpdated": false}},
	)
	if err != nil {
		log.Printf("WARNING: Failed to reset isConfigUpdated: %v", err)
	}

	return &ConfigUpdate{
		IsUpdated:    true,
		SimpleConfig: &result.SimpleConfig,
	}, nil
}
