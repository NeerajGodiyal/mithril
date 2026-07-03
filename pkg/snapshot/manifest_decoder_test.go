package snapshot

import (
	"bytes"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestManifestVoteAccountDecoderPreservesV4BLSPubkey(t *testing.T) {
	var node solana.PublicKey
	node[0] = 7
	var owner solana.PublicKey
	owner[0] = 9
	var bls [48]byte
	for i := range bls {
		bls[i] = byte(i + 1)
	}

	voteState := &sealevel.VoteStateVersions{
		Type: sealevel.VoteStateVersionV4,
		V4: sealevel.VoteState4{
			NodePubkey:          node,
			BlsPubkeyCompressed: &bls,
			LastTimestamp: sealevel.BlockTimestamp{
				Timestamp: 123,
				Slot:      456,
			},
		},
	}

	var voteStateBuf bytes.Buffer
	require.NoError(t, voteState.MarshalWithEncoder(bin.NewBinEncoder(&voteStateBuf)))

	var accountBuf bytes.Buffer
	encoder := bin.NewBinEncoder(&accountBuf)
	require.NoError(t, encoder.WriteUint64(42, bin.LE))
	require.NoError(t, encoder.WriteUint64(uint64(voteStateBuf.Len()), bin.LE))
	require.NoError(t, encoder.WriteBytes(voteStateBuf.Bytes(), false))
	require.NoError(t, encoder.WriteBytes(owner[:], false))
	require.NoError(t, encoder.WriteByte(0))
	require.NoError(t, encoder.WriteUint64(18446744073709551615, bin.LE))

	var decoded VoteAccount
	require.NoError(t, decoded.UnmarshalWithDecoder(bin.NewBinDecoder(accountBuf.Bytes())))
	require.Equal(t, node, decoded.NodePubkey)
	require.NotNil(t, decoded.BlsPubkeyCompressed)
	require.Equal(t, bls, *decoded.BlsPubkeyCompressed)
	require.Equal(t, int64(123), decoded.LastTimestampTs)
	require.Equal(t, uint64(456), decoded.LastTimestampSlot)
}
