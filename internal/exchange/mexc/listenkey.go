package mexc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"time"
)

// createListenKey creates a new listen key for user stream
func (c *Client) createListenKey(ctx context.Context) (string, error) {
	params := url.Values{}
	body, err := c.doRequest(ctx, "POST", "/api/v3/userDataStream", params, true)
	if err != nil {
		return "", fmt.Errorf("failed to create listen key: %w", err)
	}

	var resp ListenKeyResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("failed to unmarshal listen key response: %w", err)
	}

	return resp.ListenKey, nil
}

// keepAliveListenKey periodically extends the listen key validity
func (c *Client) keepAliveListenKey(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Minute) // MEXC recommends keepalive every 30-60 minutes
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Listen key keepalive stopped")
			return
		case <-ticker.C:
			if err := c.extendListenKey(ctx); err != nil {
				log.Printf("Failed to extend listen key: %v", err)
				// Try to create a new listen key
				newKey, err := c.createListenKey(ctx)
				if err != nil {
					log.Printf("Failed to create new listen key: %v", err)
					continue
				}

				// Update listen key with lock
				c.listenKeyMu.Lock()
				c.listenKey = newKey
				c.listenKeyMu.Unlock()

				log.Println("Created new listen key in keepalive")
			} else {
				log.Println("✓ Listen key extended successfully")
			}
		}
	}
}

// extendListenKey extends the validity of the current listen key
func (c *Client) extendListenKey(ctx context.Context) error {
	// Get listen key with read lock
	c.listenKeyMu.RLock()
	key := c.listenKey
	c.listenKeyMu.RUnlock()

	params := url.Values{}
	params.Set("listenKey", key)

	_, err := c.doRequest(ctx, "PUT", "/api/v3/userDataStream", params, true)
	if err != nil {
		return fmt.Errorf("failed to extend listen key: %w", err)
	}

	return nil
}

// deleteListenKey deletes the current listen key
func (c *Client) deleteListenKey(ctx context.Context) error {
	// Get listen key with read lock
	c.listenKeyMu.RLock()
	key := c.listenKey
	c.listenKeyMu.RUnlock()

	if key == "" {
		return nil // Nothing to delete
	}

	params := url.Values{}
	params.Set("listenKey", key)

	_, err := c.doRequest(ctx, "DELETE", "/api/v3/userDataStream", params, true)
	if err != nil {
		return fmt.Errorf("failed to delete listen key: %w", err)
	}

	// Clear listen key after deletion
	c.listenKeyMu.Lock()
	c.listenKey = ""
	c.listenKeyMu.Unlock()

	return nil
}
