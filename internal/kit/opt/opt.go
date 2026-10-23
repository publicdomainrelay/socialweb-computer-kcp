package opt

import "cmp"

func Positive[T cmp.Ordered](v, fallback T) T {
	var zero T
	if v > zero {
		return v
	}
	return fallback
}
