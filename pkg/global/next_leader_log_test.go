package global

import (
	"crypto/ed25519"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/leaderschedule"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatNextLeaderSuffixUsesIdentityPublicKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	identity := solana.PublicKey(pub)
	schedule := leaderschedule.NewLeaderScheduleFromKeyedSlots(map[solana.PublicKey][]uint64{
		identity: {100},
	}, 0)
	SetLeaderSchedule(schedule)
	t.Cleanup(func() { SetLeaderSchedule(nil) })

	// Must derive pubkey from private key bytes, not cast the 64-byte private key.
	got := FormatNextLeaderSuffix(solana.PrivateKey(priv).PublicKey(), 50)
	assert.Contains(t, got, "you: next 100")
	assert.NotContains(t, got, "not scheduled")

	wrong := FormatNextLeaderSuffix(solana.PublicKey(priv), 50)
	assert.Contains(t, wrong, "not scheduled")
}
