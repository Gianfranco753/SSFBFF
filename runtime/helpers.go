//go:build goexperiment.jsonv2

// Package runtime provides optional helper functions that generated code may
// use for JSONata built-in functions that are too complex to inline.
//
// Currently the transpiler inlines simple aggregations ($sum, $count) directly
// into the generated code. This package exists as an extension point for more
// complex built-ins like $reduce, $map, $filter, $string, etc.
package runtime

// SumFloat64 returns the sum of all values in the slice.
func SumFloat64(values []float64) float64 {
	var total float64
	for _, v := range values {
		total += v
	}
	return total
}

// CountFloat64 returns the number of elements in the slice.
func CountFloat64(values []float64) float64 {
	return float64(len(values))
}

// MinFloat64 returns the minimum value in the slice, or 0 if empty.
func MinFloat64(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	min := values[0]
	for _, v := range values[1:] {
		if v < min {
			min = v
		}
	}
	return min
}

// MaxFloat64 returns the maximum value in the slice, or 0 if empty.
func MaxFloat64(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	max := values[0]
	for _, v := range values[1:] {
		if v > max {
			max = v
		}
	}
	return max
}

// AverageFloat64 returns the arithmetic mean of all values, or 0 if empty.
func AverageFloat64(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	return SumFloat64(values) / float64(len(values))
}
