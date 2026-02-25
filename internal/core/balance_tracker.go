package core

import (
	"sync"

	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/types"
)

// BalanceTracker tracks account balances from WebSocket updates and REST API fallback.
// It provides cached balance state for strategy ticks.
type BalanceTracker struct {
	baseAsset  string
	quoteAsset string

	mu        sync.RWMutex
	cache     map[string]*types.Balance // asset -> balance
	hasCached bool
}

// NewBalanceTracker creates a new balance tracker for the given trading pair
func NewBalanceTracker(baseAsset, quoteAsset string) *BalanceTracker {
	return &BalanceTracker{
		baseAsset:  baseAsset,
		quoteAsset: quoteAsset,
		cache:      make(map[string]*types.Balance),
	}
}

// Get returns the current cached balance state, or nil if no cache
func (bt *BalanceTracker) Get() *BalanceState {
	bt.mu.RLock()
	defer bt.mu.RUnlock()

	if !bt.hasCached {
		return nil
	}

	state := &BalanceState{}

	if base, ok := bt.cache[bt.baseAsset]; ok {
		state.BaseFree = base.Free
		state.BaseLocked = base.Locked
	}

	if quote, ok := bt.cache[bt.quoteAsset]; ok {
		state.QuoteFree = quote.Free
		state.QuoteLocked = quote.Locked
	}

	return state
}

// UpdateFromEvent updates balances from a WebSocket account event
func (bt *BalanceTracker) UpdateFromEvent(event *types.AccountEvent) {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	for i := range event.Balances {
		b := &event.Balances[i]
		bt.cache[b.Asset] = b
	}
	bt.hasCached = true
}

// UpdateFromAccount updates balances from a REST API account response
func (bt *BalanceTracker) UpdateFromAccount(acct *exchange.Account) *BalanceState {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	state := &BalanceState{}

	for _, b := range acct.Balances {
		bt.cache[b.Asset] = &types.Balance{
			Asset:  b.Asset,
			Free:   b.Free,
			Locked: b.Locked,
		}

		if b.Asset == bt.baseAsset {
			state.BaseFree = b.Free
			state.BaseLocked = b.Locked
		}
		if b.Asset == bt.quoteAsset {
			state.QuoteFree = b.Free
			state.QuoteLocked = b.Locked
		}
	}

	bt.hasCached = true
	return state
}

// GetAssetBalance returns balance for a specific asset
func (bt *BalanceTracker) GetAssetBalance(asset string) (free, locked float64, ok bool) {
	bt.mu.RLock()
	defer bt.mu.RUnlock()

	if b, exists := bt.cache[asset]; exists {
		return b.Free, b.Locked, true
	}
	return 0, 0, false
}

// GetAllBalances returns all cached balances
func (bt *BalanceTracker) GetAllBalances() map[string]*types.Balance {
	bt.mu.RLock()
	defer bt.mu.RUnlock()

	result := make(map[string]*types.Balance, len(bt.cache))
	for k, v := range bt.cache {
		result[k] = v
	}
	return result
}

// Clear clears the balance cache
func (bt *BalanceTracker) Clear() {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	bt.cache = make(map[string]*types.Balance)
	bt.hasCached = false
}
