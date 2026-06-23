package global

import (
	"fmt"
	"time"

	"github.com/gagliardetto/solana-go"
)

// FormatNextLeaderSuffix returns a short suffix for the replay slot summary line.
// TODO(cavey-debug): remove after debugging.
func FormatNextLeaderSuffix(identity solana.PublicKey, currentSlot uint64) string {
	if identity.IsZero() {
		return ""
	}
	if leader, ok := LeaderForSlot(currentSlot); ok && leader == identity {
		return " | you: LEADING"
	}
	next, ok := NextLeaderSlotForIdentity(identity, currentSlot)
	if !ok {
		return " | you: not scheduled"
	}
	slotsUntil := next - currentSlot
	return fmt.Sprintf(" | you: next %d in %d (~%s)", next, slotsUntil, formatLeaderETA(slotsUntil))
}

func formatLeaderETA(slotsUntil uint64) string {
	d := time.Duration(slotsUntil) * slotDuration
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
