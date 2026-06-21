package txfixture

import (
	"fmt"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
)

var (
	payerWallet = solana.NewWallet()
	destWallet  = solana.NewWallet()
)

// PayerPubkey returns the funded signer used by transfer fixtures.
func PayerPubkey() solana.PublicKey {
	return payerWallet.PublicKey()
}

// DestPubkey returns the transfer destination used by transfer fixtures.
func DestPubkey() solana.PublicKey {
	return destWallet.PublicKey()
}

// TestBlockhash is the recent blockhash used by SignedTransferWire fixtures.
func TestBlockhash() solana.Hash {
	return solana.Hash{}
}

// PayerPrivateKey returns the signer used by transfer fixtures.
func PayerPrivateKey() solana.PrivateKey {
	return payerWallet.PrivateKey
}

// SignedTransferWire returns a valid signed system-transfer transaction wire.
// seq varies lamports so each transaction has a distinct signature.
func SignedTransferWire(seq uint64) ([]byte, error) {
	tx, err := solana.NewTransaction(
		[]solana.Instruction{
			system.NewTransferInstruction(1+(seq%1_000_000), payerWallet.PublicKey(), destWallet.PublicKey()).Build(),
		},
		solana.Hash{},
		solana.TransactionPayer(payerWallet.PublicKey()),
	)
	if err != nil {
		return nil, fmt.Errorf("build transfer tx: %w", err)
	}

	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if payerWallet.PublicKey().Equals(key) {
			return &payerWallet.PrivateKey
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sign transfer tx: %w", err)
	}

	return tx.MarshalBinary()
}

// MustSignedTransferWire panics if SignedTransferWire fails.
func MustSignedTransferWire(seq uint64) []byte {
	wire, err := SignedTransferWire(seq)
	if err != nil {
		panic(err)
	}
	return wire
}

// PrecomputeTransferPool builds n distinct signed transfer wire transactions.
func PrecomputeTransferPool(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = MustSignedTransferWire(uint64(i))
	}
	return out
}
