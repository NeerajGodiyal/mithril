package replay

import (
	"fmt"

	"github.com/gagliardetto/solana-go"
)

type DebugOptions struct {
	txs        map[solana.Signature]struct{}
	writeAccts map[solana.PublicKey]struct{}
}

func NewDebugOptions(txs, acctWrites []string) (*DebugOptions, error) {
	out := &DebugOptions{
		txs:        make(map[solana.Signature]struct{}, len(txs)),
		writeAccts: make(map[solana.PublicKey]struct{}, len(acctWrites)),
	}
	for _, tx := range txs {
		sig, err := solana.SignatureFromBase58(tx)
		if err != nil {
			return nil, fmt.Errorf("parsing tx signature %s: %w", tx, err)
		}
		out.txs[sig] = struct{}{}
	}
	for _, acct := range acctWrites {
		pk, err := solana.PublicKeyFromBase58(acct)
		if err != nil {
			return nil, fmt.Errorf("parsing account public key %s: %w", acct, err)
		}
		out.writeAccts[pk] = struct{}{}
	}
	return out, nil
}

func (x *DebugOptions) IsDebugTx(t solana.Signature) bool {
	_, ok := x.txs[t]
	return ok
}

func (x *DebugOptions) IsDebugWriteAccount(pk solana.PublicKey) bool {
	_, ok := x.writeAccts[pk]
	return ok
}
