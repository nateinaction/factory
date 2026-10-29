// Package sum adds optional numbers. A nil operand is a value nobody reported, so it is unknown
// rather than zero, and the total is unknown until something reports it. Both usage adapters add
// their counts and cost through this one helper.
package sum

// Add sums two optional numbers, missing only when both are. The result is the other operand's
// value when one side is missing, so a value nobody reported is never read as zero.
func Add[T int | float64](a, b *T) *T {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}

	total := *a + *b

	return &total
}
