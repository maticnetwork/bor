package sequencer

import (
	"testing"
	"time"
)

// The contention streak prices repeated stale-without-progress reconciles at
// one moment — losing head races to another writer. The defect it guards
// against: because the streak was only ever cleared inside the stale path, a
// healthy session that ended any other way (idle, watch, a transport blip)
// left it intact, so it survived across every quiet hour and each later
// lost-ack blip paid the whole accumulated ladder. On a 1s chain the 8s cap
// outlasts finality: the flush that would have completed a window is jumped
// by the backfill floor, leaving a store hole the auditor records as a reorg
// at a block that never reorged (devnet 51933). Progress must clear the
// streak whatever ended the session.
func TestAdvanceContention(t *testing.T) {
	tests := []struct {
		name       string
		progressed bool
		stale      bool
		streak     int
		wantDelay  time.Duration
		wantStreak int
	}{
		{"first stale is immediate", false, true, 0, 0, 1},
		{"second stale waits one rung", false, true, 1, reconcileBackoffMin, 2},
		{"third stale doubles", false, true, 2, 2 * reconcileBackoffMin, 3},
		{"the wait is capped", false, true, 20, reconcileBackoffMax, 21},
		{"progress on a stale session clears it", true, true, 5, 0, 0},
		{"progress on a non-stale session clears it too", true, false, 5, 0, 0},
		{"a non-stale idle session leaves the streak untouched", false, false, 5, 0, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			delay, streak := advanceContention(tt.progressed, tt.stale, tt.streak)
			if delay != tt.wantDelay || streak != tt.wantStreak {
				t.Fatalf("advanceContention(%v, %v, %d) = (%v, %d), want (%v, %d)",
					tt.progressed, tt.stale, tt.streak, delay, streak, tt.wantDelay, tt.wantStreak)
			}
		})
	}
}

// The bug in one assertion: a healthy session (progress) that does NOT end on
// a stale must still clear a streak the earlier lost-ack blips accrued, so the
// next blip reconciles at once instead of on the accumulated rung.
func TestHealthyProgressClearsAccruedContention(t *testing.T) {
	_, streak := advanceContention(false, true, 0) // blip 1
	_, streak = advanceContention(false, true, streak)
	_, streak = advanceContention(false, true, streak)
	if streak != 3 {
		t.Fatalf("three lost-ack blips left streak %d, want 3", streak)
	}

	// A healthy stretch that ends by going idle, not on a stale.
	_, streak = advanceContention(true, false, streak)
	if streak != 0 {
		t.Fatalf("healthy idle session left streak %d, want it cleared", streak)
	}

	// The next blip is immediate, not priced on the old ladder.
	delay, _ := advanceContention(false, true, streak)
	if delay != 0 {
		t.Fatalf("blip after a healthy session waited %v, want 0", delay)
	}
}
