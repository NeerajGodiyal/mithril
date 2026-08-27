package snapshot

import (
	"bytes"
	"math"
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

func TestManifestDecoderRejectsUnsupportedVariants(t *testing.T) {
	t.Run("epoch stakes", func(t *testing.T) {
		var encoded bytes.Buffer
		require.NoError(t, bin.NewBinEncoder(&encoded).WriteUint32(1, bin.LE))
		var decoded VersionedEpochStakes
		require.ErrorContains(t, decoded.UnmarshalWithDecoder(bin.NewBinDecoder(encoded.Bytes())), "unsupported epoch stakes version")
	})

	t.Run("epoch reward status", func(t *testing.T) {
		var encoded bytes.Buffer
		require.NoError(t, bin.NewBinEncoder(&encoded).WriteUint32(2, bin.LE))
		var decoded SerializableEpochRewardStatus
		require.ErrorContains(t, decoded.UnmarshalWithDecoder(bin.NewBinDecoder(encoded.Bytes())), "unsupported epoch reward status")
	})
}

func TestManifestDecoderRejectsImpossibleCountsAndTruncation(t *testing.T) {
	t.Run("blockhash count", func(t *testing.T) {
		var encoded bytes.Buffer
		encoder := bin.NewBinEncoder(&encoded)
		require.NoError(t, encoder.WriteUint64(0, bin.LE))
		require.NoError(t, encoder.WriteBool(false))
		require.NoError(t, encoder.WriteUint64(math.MaxUint64, bin.LE))
		var decoded BlockHashVec
		require.ErrorContains(t, decoded.UnmarshalWithDecoder(bin.NewBinDecoder(encoded.Bytes())), "cannot fit")
	})

	t.Run("truncated accounts fields", func(t *testing.T) {
		var encoded bytes.Buffer
		require.NoError(t, bin.NewBinEncoder(&encoded).WriteUint64(0, bin.LE))
		var decoded AccountsDbFields
		require.Error(t, decoded.UnmarshalWithDecoder(bin.NewBinDecoder(encoded.Bytes())))
	})
}

func TestManifestDecoderRejectsDuplicateStorageSlots(t *testing.T) {
	var encoded bytes.Buffer
	encoder := bin.NewBinEncoder(&encoded)
	require.NoError(t, encoder.WriteUint64(2, bin.LE))
	for range 2 {
		require.NoError(t, encoder.WriteUint64(42, bin.LE))
		require.NoError(t, encoder.WriteUint64(0, bin.LE))
	}
	var decoded AccountsDbFields
	require.ErrorContains(t, decoded.UnmarshalWithDecoder(bin.NewBinDecoder(encoded.Bytes())), "repeats storage slot")
}
