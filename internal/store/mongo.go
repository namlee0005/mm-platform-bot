package store

import (
	"context"
	"fmt"
	"log"
	"time"

	"mm-platform-engine/internal/types"

	"go.mongodb.org/mongo-driver/bson"
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

// SaveFill saves a fill event to MongoDB (upsert by orderId to avoid duplicates)
func (s *MongoStore) SaveFill(ctx context.Context, fill *types.FillEvent) error {
	collection := s.database.Collection(CollectionOrderHistory)
	filter := bson.M{"orderId": fill.OrderID}
	update := bson.M{"$set": fill}
	opts := options.Update().SetUpsert(true)

	_, err := collection.UpdateOne(ctx, filter, update, opts)
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

// SaveOrderUpdate saves an order update to MongoDB
func (s *MongoStore) SaveOrderUpdate(ctx context.Context, order *types.OrderEvent) error {
	collection := s.database.Collection(CollectionOrders)
	_, err := collection.InsertOne(ctx, order)
	if err != nil {
		return fmt.Errorf("failed to save order update: %w", err)
	}
	return nil
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
