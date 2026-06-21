package tpu

import (
	"github.com/gagliardetto/solana-go"

	"github.com/Overclock-Validator/mithril/pkg/tpu/sigverify"
)

func ParseTx(p []byte) (*solana.Transaction, error) {
	return sigverify.ParseTx(p)
}

func VerifyPacket(data []byte) bool {
	return sigverify.VerifyPacket(data)
}

func VerifyTxSig(tx *solana.Transaction) bool {
	return sigverify.VerifyTransaction(tx)
}
