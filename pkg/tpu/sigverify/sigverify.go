package sigverify

import (
	"crypto/ed25519"
	"errors"

	"github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

func ParseTx(p []byte) (tx *solana.Transaction, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.New("ParseTx panic")
		}
	}()

	tx, err = solana.TransactionFromDecoder(bin.NewBinDecoder(p))
	if err != nil {
		return nil, err
	}
	return
}

// VerifyPacket parses a wire transaction and verifies its signatures against
// static account keys. Unparseable packets and invalid signatures are discarded.
// Address lookup tables are not resolved.
func VerifyPacket(data []byte) bool {
	tx, err := ParseTx(data)
	if err != nil {
		return false
	}
	return VerifyTransaction(tx)
}

func VerifyTransaction(tx *solana.Transaction) bool {
	required := int(tx.Message.Header.NumRequiredSignatures)
	if required == 0 || len(tx.Signatures) != required || required > len(tx.Message.AccountKeys) {
		return false
	}

	msg, err := tx.Message.MarshalBinary()
	if err != nil {
		return false
	}

	keys := tx.Message.AccountKeys
	for i := 0; i < required; i++ {
		if !ed25519.Verify(keys[i][:], msg, tx.Signatures[i][:]) {
			return false
		}
	}
	return true
}
