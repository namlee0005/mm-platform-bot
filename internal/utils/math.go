package utils

import (
	"math"
)

const Epsilon = 1e-10

// RoundDown rounds a value down to the nearest multiple of step
// Used for rounding prices down to tick size
func RoundDown(value, step float64) float64 {
	if step <= 0 {
		return value
	}
	return math.Floor(value/step) * step
}

// RoundUp rounds a value up to the nearest multiple of step
// Used for rounding prices up to tick size
func RoundUp(value, step float64) float64 {
	if step <= 0 {
		return value
	}
	return math.Ceil(value/step) * step
}

// FloorToStep rounds a value down to the nearest multiple of step
// Used for rounding quantities to step size
func FloorToStep(value, step float64) float64 {
	if step <= 0 {
		return value
	}
	return math.Floor(value/step) * step
}

// Clamp restricts a value to be within [min, max]
func Clamp(value, min, max float64) float64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// ClampInt restricts an integer value to be within [min, max]
func ClampInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// Abs returns the absolute value of x
func Abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// Max returns the maximum of two floats
func Max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// Min returns the minimum of two floats
func Min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// MaxInt returns the maximum of two integers
func MaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// MinInt returns the minimum of two integers
func MinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// SafeDivide divides a by b, returning 0 if b is too small
func SafeDivide(a, b float64) float64 {
	if Abs(b) < Epsilon {
		return 0
	}
	return a / b
}

// ComputeHitImbalance computes the hit imbalance ratio from bid/ask fills
// Returns bid_notional / ask_notional with epsilon protection
func ComputeHitImbalance(bidNotional, askNotional float64) float64 {
	if bidNotional < Epsilon && askNotional < Epsilon {
		return 1.0 // No fills, balanced
	}
	return SafeDivide(bidNotional, askNotional+Epsilon)
}
