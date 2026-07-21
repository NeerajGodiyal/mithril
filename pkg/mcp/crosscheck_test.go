package mcp

import (
	"math"
	"testing"
)

func TestCompareSlots(t *testing.T) {
	cases := []struct {
		name             string
		mithril, ref, th uint64
		wantBehind       int64
		wantStatus       string
	}{
		{"in_sync within threshold", 1000, 1010, 150, 10, "in_sync"},
		{"behind over threshold", 1000, 1200, 150, 200, "behind"},
		{"exact threshold in_sync", 1000, 1150, 150, 150, "in_sync"},
		{"ahead", 1010, 1000, 150, -10, "ahead"},
		{"equal", 1000, 1000, 150, 0, "in_sync"},
		{"zero threshold one behind", 1000, 1001, 0, 1, "behind"},
		{"huge threshold no wrap", 1000, 1000, math.MaxUint64, 0, "in_sync"},
		{"extreme behind over huge threshold", 0, math.MaxUint64, uint64(math.MaxInt64) + 1, math.MaxInt64, "behind"},
		{"extreme ahead", math.MaxUint64, 0, 150, math.MinInt64, "ahead"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := compareSlots(c.mithril, c.ref, c.th, "confirmed")
			if got.SlotsBehind != c.wantBehind || got.Status != c.wantStatus {
				t.Errorf("compareSlots = behind %d status %q; want %d %q", got.SlotsBehind, got.Status, c.wantBehind, c.wantStatus)
			}
			if got.ReferenceCommitment != "confirmed" || got.MithrilView != "local_unfinalized_head" {
				t.Errorf("view semantics missing: %+v", got)
			}
		})
	}
}

func TestValidateCommitment(t *testing.T) {
	for _, ok := range []string{"processed", "confirmed", "finalized"} {
		if validateCommitment(ok) != nil {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"bogus", "", "Confirmed"} {
		if validateCommitment(bad) == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
