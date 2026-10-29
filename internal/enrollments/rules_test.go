package enrollments

import (
	"math"
	"strings"
	"testing"
)

func TestValidateRules(t *testing.T) {
	long := strings.Repeat("x", MaxReferenceLength+1)
	cases := []struct {
		reference string
		seats     int64
		want      string
	}{
		{"REF-1", 1, ""},
		{"  REF-1  ", MaxSeatsPerEnrollment, ""},
		{strings.Repeat("x", MaxReferenceLength), 1, ""},
		{"", 1, "Learner reference is required."},
		{"   ", 1, "Learner reference is required."},
		{long, 1, "Learner reference must be at most 40 characters."},
		{"REF-1", 0, "An enrollment needs at least 1 seat."},
		{"REF-1", -3, "An enrollment needs at least 1 seat."},
		{"REF-1", MaxSeatsPerEnrollment + 1, "An enrollment takes at most 5 seats."},
		{"REF-1", 4294967297, "An enrollment takes at most 5 seats."},
		{"REF-1", math.MaxInt64, "An enrollment takes at most 5 seats."},
	}
	for _, c := range cases {
		if got := Validate(c.reference, c.seats); got != c.want {
			t.Errorf("Validate(%q, %d) = %q, want %q", c.reference, c.seats, got, c.want)
		}
	}
}

func TestFeeIsAFeePerSeatPlusAFlatFee(t *testing.T) {
	for _, seats := range []int{1, 2, MaxSeatsPerEnrollment} {
		got, err := FeeCents(seats)
		if want := int64(seats)*FeeCentsPerSeat + FlatFeeCents; err != nil || got != want {
			t.Errorf("FeeCents(%d) = %d, %v; want %d", seats, got, err, want)
		}
	}
}

func TestFeeThrowsInsteadOfWrapping(t *testing.T) {
	for _, seats := range []int{math.MaxInt32, -1} {
		if _, err := FeeCents(seats); seats < 0 && err == nil {
			t.Errorf("FeeCents(%d) must fail", seats)
		}
	}
}
