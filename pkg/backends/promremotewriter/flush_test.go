package datadog

import (
	"math"
	"testing"
)

func TestCoerceToNumeric(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		arg  float64
		want float64
	}{
		{"NaN should return 0", math.NaN(), -1},
		{"+Inf should return maximum float value", math.Inf(+1), math.MaxFloat64},
		{"-Inf should return minimum float value", math.Inf(-1), -math.MaxFloat64},
		{"Zero value should return unchanged", 0, 0},
		{"Positive value within float64 range should return unchanged", 12_345, 12_345},
		{"Negative value within float64 should return unchanged", -12_345, -12_345},
		{"Maximum float64 value should return unchanged", math.MaxFloat64, math.MaxFloat64},
		{"Minimum float64 value should return unchanged", -math.MaxFloat64, -math.MaxFloat64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := coerceToNumeric(tt.arg); got != tt.want {
				t.Errorf("coerceToNumeric() = %v, want %v", got, tt.want)
			}
		})
	}
}
