package utils

import (
	"math"
	"testing"
)

func TestClamp(t *testing.T) {
	tests := []struct {
		name string
		val  float64
		min  float64
		max  float64
		want float64
	}{
		{
			name: "value within range",
			val:  5.0,
			min:  0.0,
			max:  10.0,
			want: 5.0,
		},
		{
			name: "value below min",
			val:  -5.0,
			min:  0.0,
			max:  10.0,
			want: 0.0,
		},
		{
			name: "value above max",
			val:  15.0,
			min:  0.0,
			max:  10.0,
			want: 10.0,
		},
		{
			name: "value equals min",
			val:  0.0,
			min:  0.0,
			max:  10.0,
			want: 0.0,
		},
		{
			name: "value equals max",
			val:  10.0,
			min:  0.0,
			max:  10.0,
			want: 10.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Clamp(tt.val, tt.min, tt.max)
			if got != tt.want {
				t.Errorf("Clamp(%v, %v, %v) = %v, want %v", tt.val, tt.min, tt.max, got, tt.want)
			}
		})
	}
}

func TestRoundDown(t *testing.T) {
	tests := []struct {
		name string
		val  float64
		step float64
		want float64
	}{
		{
			name: "round down to 0.01",
			val:  1.2345,
			step: 0.01,
			want: 1.23,
		},
		{
			name: "round down to 0.1",
			val:  1.25,
			step: 0.1,
			want: 1.2,
		},
		{
			name: "already aligned",
			val:  1.50,
			step: 0.01,
			want: 1.50,
		},
		{
			name: "zero step returns value",
			val:  1.23,
			step: 0,
			want: 1.23,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RoundDown(tt.val, tt.step)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("RoundDown(%v, %v) = %v, want %v", tt.val, tt.step, got, tt.want)
			}
		})
	}
}

func TestRoundUp(t *testing.T) {
	tests := []struct {
		name string
		val  float64
		step float64
		want float64
	}{
		{
			name: "round up to 0.01",
			val:  1.2345,
			step: 0.01,
			want: 1.24,
		},
		{
			name: "round up to 1.0",
			val:  15.1,
			step: 1.0,
			want: 16.0,
		},
		{
			name: "already aligned",
			val:  1.50,
			step: 0.01,
			want: 1.50,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RoundUp(tt.val, tt.step)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("RoundUp(%v, %v) = %v, want %v", tt.val, tt.step, got, tt.want)
			}
		})
	}
}

func TestFloorToStep(t *testing.T) {
	tests := []struct {
		name string
		val  float64
		step float64
		want float64
	}{
		{
			name: "floor to 0.01",
			val:  1.2345,
			step: 0.01,
			want: 1.23,
		},
		{
			name: "floor to 1.0",
			val:  5.7,
			step: 1.0,
			want: 5.0,
		},
		{
			name: "already aligned",
			val:  10.0,
			step: 1.0,
			want: 10.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FloorToStep(tt.val, tt.step)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("FloorToStep(%v, %v) = %v, want %v", tt.val, tt.step, got, tt.want)
			}
		})
	}
}

func TestSafeDivide(t *testing.T) {
	tests := []struct {
		name string
		a    float64
		b    float64
		want float64
	}{
		{
			name: "normal division",
			a:    10.0,
			b:    2.0,
			want: 5.0,
		},
		{
			name: "divide by zero returns 0",
			a:    10.0,
			b:    0.0,
			want: 0.0,
		},
		{
			name: "divide by very small number returns 0",
			a:    10.0,
			b:    1e-15,
			want: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SafeDivide(tt.a, tt.b)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("SafeDivide(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestComputeHitImbalance(t *testing.T) {
	tests := []struct {
		name        string
		bidNotional float64
		askNotional float64
		want        float64
	}{
		{
			name:        "balanced fills",
			bidNotional: 100.0,
			askNotional: 100.0,
			want:        1.0,
		},
		{
			name:        "more bid fills",
			bidNotional: 200.0,
			askNotional: 100.0,
			want:        2.0,
		},
		{
			name:        "no fills returns balanced",
			bidNotional: 0.0,
			askNotional: 0.0,
			want:        1.0,
		},
		{
			name:        "only bid fills",
			bidNotional: 100.0,
			askNotional: 0.0,
			want:        100.0 / Epsilon,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeHitImbalance(tt.bidNotional, tt.askNotional)
			if math.Abs(got-tt.want) > 1e-6 {
				t.Errorf("ComputeHitImbalance(%v, %v) = %v, want %v", tt.bidNotional, tt.askNotional, got, tt.want)
			}
		})
	}
}
