package modules

import (
	"log"
	"sync"
	"time"

	"mm-platform-engine/internal/core"
)

// StateMachine manages mode transitions with explicit rules.
type StateMachine struct {
	mu          sync.RWMutex
	currentMode core.Mode
	enteredAt   time.Time
	modeHistory []modeEntry

	// Time tracking for uptime calculation
	modeTime map[core.Mode]time.Duration
	lastTick time.Time
}

type modeEntry struct {
	mode      core.Mode
	enteredAt time.Time
	reason    string
}

func NewStateMachine() *StateMachine {
	now := time.Now()
	return &StateMachine{
		currentMode: core.ModeNormal,
		enteredAt:   now,
		modeHistory: make([]modeEntry, 0, 256),
		modeTime:    make(map[core.Mode]time.Duration),
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
) (core.Mode, bool) {
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
) core.Mode {

	// RULE 1: Drawdown limit → PAUSED (from any mode)
	if riskGuard.ShouldPause() {
		return core.ModePaused
	}

	switch sm.currentMode {
	case core.ModeNormal:
		// Shock → DEFENSIVE
		if shockDetected {
			riskGuard.EnterDefensive()
			return core.ModeDefensive
		}
		// Imbalance → RECOVERY
		if invCtrl.ShouldEnterRecovery(invDev) {
			return core.ModeRecovery
		}
		return core.ModeNormal

	case core.ModeDefensive:
		// Stay defensive until cooldown expires
		if !riskGuard.IsDefensiveCooldownExpired() {
			return core.ModeDefensive
		}
		// Cooldown expired, check if shock is gone
		if shockDetected {
			riskGuard.EnterDefensive()
			return core.ModeDefensive
		}
		// Cooldown expired + no shock → check imbalance
		if invCtrl.ShouldEnterRecovery(invDev) {
			return core.ModeRecovery
		}
		return core.ModeNormal

	case core.ModeRecovery:
		// Shock → DEFENSIVE (even from recovery)
		if shockDetected {
			riskGuard.EnterDefensive()
			return core.ModeDefensive
		}
		// Imbalance recovered → NORMAL
		if invCtrl.ShouldExitRecovery(invDev) {
			return core.ModeNormal
		}
		return core.ModeRecovery

	case core.ModePaused:
		// Only exit PAUSED if drawdown recovers
		if !riskGuard.ShouldPause() {
			if invCtrl.ShouldEnterRecovery(invDev) {
				return core.ModeRecovery
			}
			return core.ModeNormal
		}
		return core.ModePaused
	}

	return core.ModeNormal
}

func (sm *StateMachine) transitionReason(from, to core.Mode, shockInfo ShockInfo, invDev float64, rg *RiskGuard) string {
	switch {
	case to == core.ModePaused:
		return "drawdown limit exceeded"
	case to == core.ModeDefensive && shockInfo.Detected:
		return "shock: " + shockInfo.String()
	case to == core.ModeRecovery:
		return "inventory imbalance exceeds threshold"
	case from == core.ModeDefensive && to == core.ModeNormal:
		return "defensive cooldown expired, no shock"
	case from == core.ModeRecovery && to == core.ModeNormal:
		return "inventory recovered to target"
	case from == core.ModePaused && to == core.ModeNormal:
		return "drawdown recovered"
	case from == core.ModePaused && to == core.ModeRecovery:
		return "drawdown recovered but still imbalanced"
	default:
		return "transition"
	}
}

// CurrentMode returns the current mode (thread-safe)
func (sm *StateMachine) CurrentMode() core.Mode {
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
func (sm *StateMachine) QuoteUptime() float64 {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var totalActive, totalPaused time.Duration
	for mode, dur := range sm.modeTime {
		if mode == core.ModePaused {
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

// ModeTimeSummary returns seconds spent in each mode
func (sm *StateMachine) ModeTimeSummary() map[core.Mode]float64 {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	result := make(map[core.Mode]float64, len(sm.modeTime))
	for mode, dur := range sm.modeTime {
		result[mode] = dur.Seconds()
	}
	return result
}

// ForceMode manually sets the mode
func (sm *StateMachine) ForceMode(mode core.Mode, reason string) {
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
