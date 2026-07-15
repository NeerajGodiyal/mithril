package turbine_test

import (
	_ "embed"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/rewardcerts"
	"github.com/Overclock-Validator/mithril/pkg/turbine"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

//go:embed testdata/agave_block_footer_with_reward_certs.bin
var agaveBlockFooterWithRewardCerts []byte

func TestBlockFooterMarkerMatchesAgaveWincodeFixture(t *testing.T) {
	var skipSig [96]byte
	for i := range skipSig {
		skipSig[i] = 0x35
	}
	skipRaw, err := rewardcerts.EncodeSkipRewardCertificate(rewardcerts.SkipRewardCertificate{
		Slot:      3676604,
		Signature: skipSig,
		Bitmap:    []byte{0x01, 0x02, 0x03},
	})
	require.NoError(t, err)

	var notarSig [96]byte
	for i := range notarSig {
		notarSig[i] = 0x55
	}
	var blockID solana.Hash
	for i := range blockID {
		blockID[i] = 0x44
	}
	notarRaw, err := rewardcerts.EncodeNotarRewardCertificate(rewardcerts.NotarRewardCertificate{
		Slot:      3676604,
		BlockID:   blockID,
		Signature: notarSig,
		Bitmap:    []byte{0x04, 0x05},
	})
	require.NoError(t, err)

	var bankHash solana.Hash
	for i := range bankHash {
		bankHash[i] = 0x22
	}
	component := turbine.NewBlockFooter(turbine.BlockFooter{
		BankHash:               bankHash,
		BlockProducerTimeNanos: 42,
		BlockUserAgent:         []byte("mithril"),
		SkipRewardCert:         skipRaw,
		NotarRewardCert:        notarRaw,
	})

	got, err := turbine.MarshalBlockComponent(component)
	require.NoError(t, err)
	require.Equal(t, agaveBlockFooterWithRewardCerts, got)
}
