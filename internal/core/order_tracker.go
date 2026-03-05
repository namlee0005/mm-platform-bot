package core

import (
	"sync"
)

// OrderTracker tracks live orders on the exchange.
// It syncs from WebSocket updates and provides order state for strategy ticks.
type OrderTracker struct {
	mu     sync.RWMutex
	orders map[string]*LiveOrder // orderID -> order
}

// NewOrderTracker creates a new order tracker
func NewOrderTracker() *OrderTracker {
	return &OrderTracker{
		orders: make(map[string]*LiveOrder),
	}
}

// Add adds or updates an order
func (ot *OrderTracker) Add(order *LiveOrder) {
	ot.mu.Lock()
	defer ot.mu.Unlock()
	ot.orders[order.OrderID] = order
}

// Remove removes an order by ID
func (ot *OrderTracker) Remove(orderID string) {
	ot.mu.Lock()
	defer ot.mu.Unlock()
	delete(ot.orders, orderID)
}

// Get returns an order by ID, or nil if not found
func (ot *OrderTracker) Get(orderID string) *LiveOrder {
	ot.mu.RLock()
	defer ot.mu.RUnlock()
	return ot.orders[orderID]
}

// GetAll returns all live orders as a slice
func (ot *OrderTracker) GetAll() []LiveOrder {
	ot.mu.RLock()
	defer ot.mu.RUnlock()

	orders := make([]LiveOrder, 0, len(ot.orders))
	for _, o := range ot.orders {
		orders = append(orders, *o)
	}
	return orders
}

// GetBySide returns all orders on a specific side
func (ot *OrderTracker) GetBySide(side string) []LiveOrder {
	ot.mu.RLock()
	defer ot.mu.RUnlock()

	orders := make([]LiveOrder, 0, len(ot.orders)/2)
	for _, o := range ot.orders {
		if o.Side == side {
			orders = append(orders, *o)
		}
	}
	return orders
}

// Count returns the number of live orders
func (ot *OrderTracker) Count() int {
	ot.mu.RLock()
	defer ot.mu.RUnlock()
	return len(ot.orders)
}

// CountBySide returns the number of orders on a specific side
func (ot *OrderTracker) CountBySide(side string) int {
	ot.mu.RLock()
	defer ot.mu.RUnlock()

	count := 0
	for _, o := range ot.orders {
		if o.Side == side {
			count++
		}
	}
	return count
}

// UpdateRemaining updates the remaining quantity of an order
func (ot *OrderTracker) UpdateRemaining(orderID string, remaining float64) {
	ot.mu.Lock()
	defer ot.mu.Unlock()

	if order, ok := ot.orders[orderID]; ok {
		order.RemainingQty = remaining
	}
}

// Clear removes all orders
func (ot *OrderTracker) Clear() {
	ot.mu.Lock()
	defer ot.mu.Unlock()
	ot.orders = make(map[string]*LiveOrder)
}

// TotalNotional returns total notional value of all orders within a price range
func (ot *OrderTracker) TotalNotional(mid, rangePct float64) (bidNotional, askNotional float64) {
	ot.mu.RLock()
	defer ot.mu.RUnlock()

	rangeAbs := mid * rangePct

	for _, o := range ot.orders {
		notional := o.Price * o.RemainingQty

		if o.Side == "BUY" && o.Price >= mid-rangeAbs {
			bidNotional += notional
		}
		if o.Side == "SELL" && o.Price <= mid+rangeAbs {
			askNotional += notional
		}
	}

	return
}

// HasOrderNearPrice checks if there's an order near a specific price
func (ot *OrderTracker) HasOrderNearPrice(side string, price, tolerance float64) bool {
	ot.mu.RLock()
	defer ot.mu.RUnlock()

	for _, o := range ot.orders {
		if o.Side == side {
			diff := o.Price - price
			if diff < 0 {
				diff = -diff
			}
			if diff <= tolerance {
				return true
			}
		}
	}
	return false
}
