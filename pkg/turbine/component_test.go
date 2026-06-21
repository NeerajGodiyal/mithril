package turbine_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/turbine"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func testLeader(t *testing.T) solana.PrivateKey {
	t.Helper()
	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	return solana.PrivateKey(ed25519.NewKeyFromSeed(seed[:]))
}

func TestBlockComponentEntryBatchRoundTrip(t *testing.T) {
	entry := turbine.Entry{
		NumHashes: 1,
		Hash:      solana.Hash{9},
	}
	component, err := turbine.NewEntryBatch([]turbine.Entry{entry})
	require.NoError(t, err)

	raw, err := turbine.MarshalBlockComponent(component)
	require.NoError(t, err)
	decoded, err := turbine.UnmarshalBlockComponent(raw)
	require.NoError(t, err)
	require.True(t, decoded.IsEntryBatch())
	require.Len(t, decoded.EntryBatch, 1)
	require.Equal(t, entry.NumHashes, decoded.EntryBatch[0].NumHashes)
	require.Equal(t, entry.Hash, decoded.EntryBatch[0].Hash)
}

func TestBlockComponentMarkerRoundTrip(t *testing.T) {
	parentID := solana.Hash{7}
	component := turbine.NewBlockHeader(99, parentID)

	raw, err := turbine.MarshalBlockComponent(component)
	require.NoError(t, err)
	require.True(t, turbine.InferIsBlockMarker(raw))

	decoded, err := turbine.UnmarshalBlockComponent(raw)
	require.NoError(t, err)
	require.True(t, decoded.IsMarker())
	require.Equal(t, turbine.MarkerBlockHeader, decoded.Marker.Kind)
	require.Equal(t, uint64(99), decoded.Marker.Header.ParentSlot)
	require.Equal(t, parentID, decoded.Marker.Header.ParentBlockID)
}

func TestShredEntryBatchRoundTrip(t *testing.T) {
	leader := testLeader(t)
	entry := turbine.Entry{NumHashes: 1, Hash: solana.Hash{3}}
	component, err := turbine.NewEntryBatch([]turbine.Entry{entry})
	require.NoError(t, err)

	shredder := turbine.Shredder{
		Slot:          100,
		ParentSlot:    95,
		Version:       42,
		ReferenceTick: 5,
	}
	batch, _, _, err := shredder.MakeMerkleShredsFromComponent(
		leader,
		component,
		true,
		solana.Hash{1},
		0,
		0,
	)
	require.NoError(t, err)
	require.NotEmpty(t, batch.DataShreds)
	require.NotEmpty(t, batch.CodeShreds)

	leaderPub := leader.PublicKey()
	for _, shred := range append(batch.DataShreds, batch.CodeShreds...) {
		require.NoError(t, shred.VerifySignature(leaderPub))
	}

	components, err := turbine.DecodeComponentsFromDataShreds(batch.DataShreds)
	require.NoError(t, err)
	require.Len(t, components, 1)
	require.True(t, components[0].IsEntryBatch())
	require.Equal(t, entry.NumHashes, components[0].EntryBatch[0].NumHashes)
}

func TestShredBlockHeaderMarkerRoundTrip(t *testing.T) {
	leader := testLeader(t)
	parentID := solana.Hash{8}
	component := turbine.NewBlockHeader(95, parentID)

	shredder := turbine.Shredder{Slot: 100, ParentSlot: 95, Version: 1}
	batch, _, _, err := shredder.MakeMerkleShredsFromComponent(
		leader,
		component,
		false,
		solana.Hash{2},
		0,
		0,
	)
	require.NoError(t, err)

	components, err := turbine.DecodeComponentsFromDataShreds(batch.DataShreds)
	require.NoError(t, err)
	require.Len(t, components, 1)
	require.True(t, components[0].IsMarker())
	require.Equal(t, parentID, components[0].Marker.Header.ParentBlockID)
}

func TestShredBlockFooterMarkerRoundTrip(t *testing.T) {
	leader := testLeader(t)
	footer := turbine.BlockFooter{
		BankHash:               solana.Hash{4},
		BlockProducerTimeNanos: 123,
		BlockUserAgent:         []byte("mithril"),
	}
	component := turbine.NewBlockFooter(footer)

	shredder := turbine.Shredder{Slot: 200, ParentSlot: 199, Version: 1, ReferenceTick: 63}
	batch, _, _, err := shredder.MakeMerkleShredsFromComponent(
		leader,
		component,
		true,
		solana.Hash{3},
		10,
		10,
	)
	require.NoError(t, err)

	components, err := turbine.DecodeComponentsFromDataShreds(batch.DataShreds)
	require.NoError(t, err)
	require.Len(t, components, 1)
	require.Equal(t, footer.BankHash, components[0].Marker.Footer.BankHash)
	require.Equal(t, footer.BlockUserAgent, components[0].Marker.Footer.BlockUserAgent)
}
