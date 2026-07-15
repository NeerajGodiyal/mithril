package pipeline

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
)

func mustValidTestWire(tb testing.TB) []byte {
	tb.Helper()
	wire := txfixture.MustSignedTransferWire(0)
	if !verifyPacket(wire) {
		tb.Fatal("fixture must verify")
	}
	return wire
}
