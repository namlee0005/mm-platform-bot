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
	CollectionOrders       = "orders"
)

// MongoStore handles persisting data to MongoDB
type MongoStore struct {
	client   *mongo.Client
	database *mongo.Database
}

// NewMongoStore creates a new MongoDB store
func NewMongoStore(uri, database string) (*MongoStore, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to mongodb: %w", err)
	}

	// Ping to verify connection
	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("failed to ping mongodb: %w", err)
	}

	db := client.Database(database)

	// Ensure unique index on orderId for order_history collection
	orderHistoryCollection := db.Collection(CollectionOrderHistory)
	indexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "orderId", Value: 1}},
		Options: options.Index().SetUnique(true),
	}
	if _, err := orderHistoryCollection.Indexes().CreateOne(ctx, indexModel); err != nil {
		log.Printf("Warning: failed to create unique index on orderId: %v", err)
		// Continue despite error - index might already exist
	}

	return &MongoStore{
		client:   client,
		database: db,
	}, nil
}

// SaveFill saves a fill event to MongoDB.
// Uses InsertOne to preserve all partial fills (not upsert which overwrites).
// Each fill is a separate document — partial fills for the same orderId
// are stored as individual records with different quantities.
func (s *MongoStore) SaveFill(ctx context.Context, fill *types.FillEvent) error {
	collection := s.database.Collection(CollectionOrderHistory)
	_, err := collection.InsertOne(ctx, fill)
	if err != nil {
		return fmt.Errorf("failed to save fill: %w", err)
	}
	return nil
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

// UpsertFilledOrder saves or updates a filled order in MongoDB.
// Uses orderId as unique key to avoid duplicates from repeated syncs.
func (s *MongoStore) UpsertFilledOrder(ctx context.Context, order *types.OrderEvent) error {
	collection := s.database.Collection(CollectionOrders)
	filter := bson.M{"orderId": order.OrderID}
	update := bson.M{"$set": order}
	opts := options.Update().SetUpsert(true)
	_, err := collection.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return fmt.Errorf("failed to upsert filled order: %w", err)
	}
	return nil
}

// GetLatestOrderTimestamp returns the timestamp of the most recent order in the orders collection.
// Used to determine the `since` parameter for FetchClosedOrders.
func (s *MongoStore) GetLatestOrderTimestamp(ctx context.Context, symbol string) (time.Time, error) {
	collection := s.database.Collection(CollectionOrders)
	filter := bson.M{}
	if symbol != "" {
		filter["symbol"] = symbol
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

// GetRecentFills retrieves recent fills from MongoDB
func (s *MongoStore) GetRecentFills(ctx context.Context, symbol string, limit int64) ([]*types.FillEvent, error) {
	collection := s.database.Collection(CollectionOrderHistory)
	filter := bson.M{"symbol": symbol}
	opts := options.Find().SetSort(bson.D{{Key: "timestamp", Value: -1}}).SetLimit(limit)

	cursor, err := collection.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to query fills: %w", err)
	}
	defer cursor.Close(ctx)

	var fills []*types.FillEvent
	if err := cursor.All(ctx, &fills); err != nil {
		return nil, fmt.Errorf("failed to decode fills: %w", err)
	}

	return fills, nil
}

// GetFillsInWindow retrieves fills within a time window (from sinceTime to now)
func (s *MongoStore) GetFillsInWindow(ctx context.Context, symbol string, sinceTime time.Time) ([]*types.FillEvent, error) {
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

	var fills []*types.FillEvent
	if err := cursor.All(ctx, &fills); err != nil {
		return nil, fmt.Errorf("failed to decode fills: %w", err)
	}

	return fills, nil
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
