package txverify

import (
	"fmt"

	"github.com/gagliardetto/solana-go"
)

func MessageBytes(tx *solana.Transaction) ([]byte, error) {
	if tx == nil {
		return nil, fmt.Errorf("nil transaction")
	}
	msg, err := tx.Message.MarshalBinary()
	if err != nil {
		return nil, err
	}
	if tx.Message.IsVersioned() {
		if len(msg) == 0 {
			return nil, fmt.Errorf("empty versioned message")
		}
		version := byte(tx.Message.GetVersion())
		if version == 0 {
			msg[0] = 0x80
		} else {
			msg[0] = 0x7f + version
		}
	}
	return msg, nil
}

func VerifyTransaction(tx *solana.Transaction) error {
	msg, err := MessageBytes(tx)
	if err != nil {
		return err
	}

	signers := tx.Message.Signers()
	if len(signers) != len(tx.Signatures) {
		return fmt.Errorf("got %d signers, but %d signatures", len(signers), len(tx.Signatures))
	}

	for i, sig := range tx.Signatures {
		if !sig.Verify(signers[i], msg) {
			return fmt.Errorf("invalid signature by %s", signers[i])
		}
	}
	return nil
}
