package store

import (
	"context"
	"fmt"
	"time"

	"mm-platform-engine/internal/types"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoStore handles persisting data to MongoDB
type MongoStore struct {
	client     *mongo.Client
	database   *mongo.Database
	collection *mongo.Collection
}

// NewMongoStore creates a new MongoDB store
func NewMongoStore(uri, database, collection string) (*MongoStore, error) {
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
	coll := db.Collection(collection)

	return &MongoStore{
		client:     client,
		database:   db,
		collection: coll,
	}, nil
}

// SaveFill saves a fill event to MongoDB
func (s *MongoStore) SaveFill(ctx context.Context, fill *types.FillEvent) error {
	_, err := s.collection.InsertOne(ctx, fill)
	if err != nil {
		return fmt.Errorf("failed to save fill: %w", err)
	}
	return nil
}

// SaveDeal saves a completed deal to MongoDB
func (s *MongoStore) SaveDeal(ctx context.Context, deal *types.DealEvent) error {
	dealCollection := s.database.Collection("deals")
	_, err := dealCollection.InsertOne(ctx, deal)
	if err != nil {
		return fmt.Errorf("failed to save deal: %w", err)
	}
	return nil
}

// SaveOrderUpdate saves an order update to MongoDB
func (s *MongoStore) SaveOrderUpdate(ctx context.Context, order *types.OrderEvent) error {
	orderCollection := s.database.Collection("orders")
	_, err := orderCollection.InsertOne(ctx, order)
	if err != nil {
		return fmt.Errorf("failed to save order update: %w", err)
	}
	return nil
}

// GetRecentFills retrieves recent fills from MongoDB
func (s *MongoStore) GetRecentFills(ctx context.Context, symbol string, limit int64) ([]*types.FillEvent, error) {
	filter := map[string]interface{}{"symbol": symbol}
	opts := options.Find().SetSort(map[string]interface{}{"timestamp": -1}).SetLimit(limit)

	cursor, err := s.collection.Find(ctx, filter, opts)
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

// Close closes the MongoDB connection
func (s *MongoStore) Close(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}
