package block

import (
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/rpcpool/yellowstone-grpc/examples/golang/proto"
	"github.com/stretchr/testify/require"
)

func TestLaserStreamVersionFlagMapsToV0(t *testing.T) {
	legacy := lsTransactionToTransaction(&proto.Transaction{Message: &proto.Message{
		Header:          &proto.MessageHeader{},
		RecentBlockhash: make([]byte, len(solana.Hash{})),
	}})
	require.Equal(t, solana.MessageVersionLegacy, legacy.Message.GetVersion())

	v0 := lsTransactionToTransaction(&proto.Transaction{Message: &proto.Message{
		Header:          &proto.MessageHeader{},
		RecentBlockhash: make([]byte, len(solana.Hash{})),
		Versioned:       true,
	}})
	require.Equal(t, solana.MessageVersionV0, v0.Message.GetVersion())
}

func TestLaserStreamPreservesLoadedAddressRoles(t *testing.T) {
	readonly := solana.PublicKey{1}
	writable := solana.PublicKey{2}
	meta := lsTxMetaToTxMeta(&proto.TransactionStatusMeta{
		LoadedReadonlyAddresses: [][]byte{readonly[:]},
		LoadedWritableAddresses: [][]byte{writable[:]},
	})
	require.Equal(t, solana.PublicKeySlice{readonly}, meta.LoadedAddresses.ReadOnly)
	require.Equal(t, solana.PublicKeySlice{writable}, meta.LoadedAddresses.Writable)
}
