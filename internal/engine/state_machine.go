package engine

import (
	"log"
	"sync"
	"time"
)

// StateMachine manages mode transitions with explicit rules.
type StateMachine struct {
	mu          sync.RWMutex
	currentMode Mode
	enteredAt   time.Time
	modeHistory []modeEntry

	// Time tracking for uptime calculation
	modeTime map[Mode]time.Duration
	lastTick time.Time
}

type modeEntry struct {
	mode      Mode
	enteredAt time.Time
	reason    string
}

func NewStateMachine() *StateMachine {
	now := time.Now()
	return &StateMachine{
		currentMode: ModeNormal,
		enteredAt:   now,
		modeHistory: make([]modeEntry, 0, 256),
		modeTime:    make(map[Mode]time.Duration),
		lastTick:    now,
	}
}

// Evaluate determines the correct mode based on current conditions.
// Returns the mode and whether a transition occurred.
func (sm *StateMachine) Evaluate(
	shockInfo ShockInfo,
	invDev float64,
	riskGuard *RiskGuard,
	invCtrl *InventoryController,
	priceEngine *PriceEngine,
) (Mode, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	prevMode := sm.currentMode

	// Track time in previous mode
	elapsed := now.Sub(sm.lastTick)
	sm.modeTime[prevMode] += elapsed
	sm.lastTick = now

	newMode := sm.computeTransition(shockInfo.Detected, invDev, riskGuard, invCtrl, priceEngine)

	if newMode != prevMode {
		reason := sm.transitionReason(prevMode, newMode, shockInfo, invDev, riskGuard)
		sm.currentMode = newMode
		sm.enteredAt = now
		sm.modeHistory = append(sm.modeHistory, modeEntry{
			mode:      newMode,
			enteredAt: now,
			reason:    reason,
		})

		// Trim history
		if len(sm.modeHistory) > 256 {
			sm.modeHistory = sm.modeHistory[len(sm.modeHistory)-128:]
		}

		log.Printf("[STATE] %s → %s (reason: %s)", prevMode, newMode, reason)
		return newMode, true
	}

	return prevMode, false
}

func (sm *StateMachine) computeTransition(
	shockDetected bool,
	invDev float64,
	riskGuard *RiskGuard,
	invCtrl *InventoryController,
	priceEngine *PriceEngine,
) Mode {

	// RULE 1: Drawdown limit → PAUSED (from any mode)
	if riskGuard.ShouldPause() {
		return ModePaused
	}

	switch sm.currentMode {
	case ModeNormal:
		// Shock → DEFENSIVE
		if shockDetected {
			riskGuard.EnterDefensive()
			return ModeDefensive
		}
		// Imbalance → RECOVERY
		if invCtrl.ShouldEnterRecovery(invDev) {
			return ModeRecovery
		}
		return ModeNormal

	case ModeDefensive:
		// Stay defensive until cooldown expires
		if !riskGuard.IsDefensiveCooldownExpired() {
			return ModeDefensive
		}
		// Cooldown expired, check if shock is gone
		if shockDetected {
			// Reset cooldown
			riskGuard.EnterDefensive()
			return ModeDefensive
		}
		// Cooldown expired + no shock → check imbalance
		if invCtrl.ShouldEnterRecovery(invDev) {
			return ModeRecovery
		}
		return ModeNormal

	case ModeRecovery:
		// Shock → DEFENSIVE (even from recovery)
		if shockDetected {
			riskGuard.EnterDefensive()
			return ModeDefensive
		}
		// Imbalance recovered → NORMAL
		if invCtrl.ShouldExitRecovery(invDev) {
			return ModeNormal
		}
		return ModeRecovery

	case ModePaused:
		// Only exit PAUSED if drawdown recovers
		if !riskGuard.ShouldPause() {
			// Check if still imbalanced
			if invCtrl.ShouldEnterRecovery(invDev) {
				return ModeRecovery
			}
			return ModeNormal
		}
		return ModePaused
	}

	return ModeNormal
}

func (sm *StateMachine) transitionReason(from, to Mode, shockInfo ShockInfo, invDev float64, rg *RiskGuard) string {
	switch {
	case to == ModePaused:
		return "drawdown limit exceeded"
	case to == ModeDefensive && shockInfo.Detected:
		// Include detailed shock info for monitoring
		return "shock: " + shockInfo.String()
	case to == ModeRecovery:
		return "inventory imbalance exceeds threshold"
	case from == ModeDefensive && to == ModeNormal:
		return "defensive cooldown expired, no shock"
	case from == ModeRecovery && to == ModeNormal:
		return "inventory recovered to target"
	case from == ModePaused && to == ModeNormal:
		return "drawdown recovered"
	case from == ModePaused && to == ModeRecovery:
		return "drawdown recovered but still imbalanced"
	default:
		return "transition"
	}
}

// CurrentMode returns the current mode (thread-safe)
func (sm *StateMachine) CurrentMode() Mode {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.currentMode
}

// TimeInMode returns how long the engine has been in the current mode
func (sm *StateMachine) TimeInMode() time.Duration {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return time.Since(sm.enteredAt)
}

// QuoteUptime returns the fraction of time spent quoting (not PAUSED)
// over the specified window.
func (sm *StateMachine) QuoteUptime() float64 {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var totalActive, totalPaused time.Duration
	for mode, dur := range sm.modeTime {
		if mode == ModePaused {
			totalPaused += dur
		} else {
			totalActive += dur
		}
	}

	total := totalActive + totalPaused
	if total == 0 {
		return 1.0
	}
	return float64(totalActive) / float64(total)
}

// ModeTimeSummary returns seconds spent in each mode (for reporting)
func (sm *StateMachine) ModeTimeSummary() map[Mode]float64 {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	result := make(map[Mode]float64, len(sm.modeTime))
	for mode, dur := range sm.modeTime {
		result[mode] = dur.Seconds()
	}
	return result
}

// ForceMode manually sets the mode (for admin/emergency use)
func (sm *StateMachine) ForceMode(mode Mode, reason string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(sm.lastTick)
	sm.modeTime[sm.currentMode] += elapsed
	sm.lastTick = now

	sm.currentMode = mode
	sm.enteredAt = now
	sm.modeHistory = append(sm.modeHistory, modeEntry{
		mode:      mode,
		enteredAt: now,
		reason:    "MANUAL: " + reason,
	})
	log.Printf("[STATE] MANUAL transition to %s: %s", mode, reason)
}
