package replay

import (
	"errors"
	"fmt"
	"testing"
)

func TestConfirmedDivergenceSame(t *testing.T) {
	a := &ConfirmedDivergence{Slot: 5, Ours: [32]byte{1}, Confirmed: [32]byte{2}}
	if !a.Same(&ConfirmedDivergence{Slot: 5, Ours: [32]byte{1}, Confirmed: [32]byte{2}}) {
		t.Fatal("identical divergence must compare Same")
	}
	if a.Same(nil) {
		t.Fatal("nil is never Same")
	}
	if a.Same(&ConfirmedDivergence{Slot: 6, Ours: [32]byte{1}, Confirmed: [32]byte{2}}) {
		t.Fatal("different slot is not Same")
	}
	if a.Same(&ConfirmedDivergence{Slot: 5, Ours: [32]byte{9}, Confirmed: [32]byte{2}}) {
		t.Fatal("different our-hash is not Same")
	}
}

// The retry loop unwraps via errors.As, so the typed error must survive wrapping.
func TestConfirmedDivergenceErrorsAs(t *testing.T) {
	err := fmt.Errorf("replay stopped: %w", &ConfirmedDivergence{Slot: 7})
	var div *ConfirmedDivergence
	if !errors.As(err, &div) || div.Slot != 7 {
		t.Fatalf("errors.As must recover the divergence: %v", div)
	}
}
