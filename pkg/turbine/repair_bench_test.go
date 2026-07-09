package turbine

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/gossip"
	repairproto "github.com/Overclock-Validator/mithril/pkg/repair"
)

// BenchmarkRepairRequestSign measures the per-request random-nonce + Ed25519
// signing cost, which is the practical single-core ceiling on repair send
// throughput. sendShredAttempt now signs OUTSIDE the client mutex (so it no
// longer serializes against response processing / the UDP receive loop), but
// signing is still serial within the scan goroutine — hence repairMaxSendsPerScan
// and the "~33k/s is signing-bound" note. Run: go test -run x -bench Sign ./pkg/turbine/
func BenchmarkRepairRequestSign(b *testing.B) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	var recipient gossip.Pubkey
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := repairproto.NewWindowIndexRequest(priv, recipient, 100, uint64(i)); err != nil {
			b.Fatal(err)
		}
	}
}
