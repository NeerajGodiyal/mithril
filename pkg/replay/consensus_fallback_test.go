package replay

import (
	"testing"

	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/blockstream"
)

func TestShouldDiscardLightbringerObservationAfterFallback(t *testing.T) {
	lightbringerBlock := &b.Block{Slot: 123, FromLightbringer: true}

	if !shouldDiscardLightbringerObservationAfterFallback(true, true, lightbringerBlock, blockstream.FetchStatsSnapshot{
		IsNearTip:     false,
		CurrentSource: "rpc",
	}) {
		t.Fatalf("expected Lightbringer observation to be discarded after catchup fallback")
	}

	if !shouldDiscardLightbringerObservationAfterFallback(true, true, lightbringerBlock, blockstream.FetchStatsSnapshot{
		IsNearTip:     true,
		CurrentSource: "rpc",
	}) {
		t.Fatalf("expected Lightbringer observation to be discarded while near-tip has not handed back to Lightbringer")
	}

	if shouldDiscardLightbringerObservationAfterFallback(true, true, lightbringerBlock, blockstream.FetchStatsSnapshot{
		IsNearTip:     true,
		CurrentSource: "lightbringer",
	}) {
		t.Fatalf("expected active Lightbringer observations to be retained")
	}

	if shouldDiscardLightbringerObservationAfterFallback(true, true, &b.Block{Slot: 123}, blockstream.FetchStatsSnapshot{
		IsNearTip:     false,
		CurrentSource: "rpc",
	}) {
		t.Fatalf("expected RPC block to be retained")
	}
}
