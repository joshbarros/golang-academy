// Package enrollments is the HTTP side of the saga and the handler that moves an enrollment forward as events arrive.
package enrollments

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Enrollment rules.
const (
	MaxSeatsPerEnrollment = 5
	MaxReferenceLength    = 40
	FeeCentsPerSeat       = 4_900
	FlatFeeCents          = 500
)

// ErrOverflow is returned instead of wrapping.
var ErrOverflow = errors.New("arithmetic overflow")

// Validate returns an error message, or "" when the request is acceptable. The seat count is compared as int64 on purpose:
// narrowing 4294967297 to int32 would wrap to 1 and pass.
func Validate(reference string, seats int64) string {
	trimmed := strings.TrimSpace(reference)
	switch {
	case trimmed == "":
		return "Learner reference is required."
	case len([]rune(trimmed)) > MaxReferenceLength:
		return fmt.Sprintf("Learner reference must be at most %d characters.", MaxReferenceLength)
	case seats < 1:
		return "An enrollment needs at least 1 seat."
	case seats > MaxSeatsPerEnrollment:
		return fmt.Sprintf("An enrollment takes at most %d seats.", MaxSeatsPerEnrollment)
	}
	return ""
}

// FeeCents is a fee per seat plus a flat fee, in cents. Overflow is an error, not a wrap.
func FeeCents(seats int) (int64, error) {
	if seats < 0 || int64(seats) > (math.MaxInt64-FlatFeeCents)/FeeCentsPerSeat {
		return 0, ErrOverflow
	}
	return int64(seats)*FeeCentsPerSeat + FlatFeeCents, nil
}
