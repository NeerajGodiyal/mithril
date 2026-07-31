package txverify

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/sigverify"
	"github.com/gagliardetto/solana-go"
)

// MessageBytes returns the exact bytes a transaction's signatures are computed
// over.
//
// The version-byte fixup is load-bearing and easy to omit. solana-go's
// MarshalV0 writes versionNum+127, i.e. 0x7f for a v0 message, which is not the
// Solana wire encoding; the signed prefix is 0x80. Marshalling without this
// correction produces bytes that no honest signature will ever verify against,
// so every verifier must come through here rather than calling MarshalBinary
// directly.
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

// VerifyTransaction verifies one transaction's signatures.
//
// Prefer BatchVerifier where more than a few transactions are available: a
// transaction carries one or two signatures, and verifying one or two at a time
// leaves most of a vector group idle.
func VerifyTransaction(tx *solana.Transaction) error {
	msg, err := MessageBytes(tx)
	if err != nil {
		return err
	}

	signers := tx.Message.Signers()
	if len(signers) != len(tx.Signatures) {
		return fmt.Errorf("got %d signers, but %d signatures", len(signers), len(tx.Signatures))
	}

	for i := range tx.Signatures {
		if !sigverify.VerifyOne((*[32]byte)(&signers[i]), msg, tx.Signatures[i][:]) {
			return fmt.Errorf("invalid signature by %s", signers[i])
		}
	}
	return nil
}

// BatchVerifier verifies many transactions per call. It is reusable
// caller-owned scratch and is not safe for concurrent use; give each worker
// its own.
type BatchVerifier struct {
	batch sigverify.Batch
	// counts[i] is how many signature lanes transaction i contributed, which
	// is zero when it failed a precheck. It is what maps a lane verdict back
	// to the transaction that produced it.
	counts []int
	// signers[i] is retained so a failure can name the signer without
	// recomputing Signers(), which allocates.
	signers [][]solana.PublicKey
}

// Verify checks every transaction in txs and writes a per-transaction result
// into errs, which must be the same length as txs. A nil entry means that
// transaction verified.
//
// Every transaction gets an independent verdict: one bad transaction does not
// mask the others, so a caller can report precisely which one failed.
func (v *BatchVerifier) Verify(txs []*solana.Transaction, errs []error) {
	if len(errs) != len(txs) {
		panic("txverify: errs and txs length mismatch")
	}
	v.batch.Reset()
	v.counts = v.counts[:0]
	clear(v.signers)
	v.signers = v.signers[:0]

	for i, tx := range txs {
		errs[i] = nil
		signers, msg, err := prepare(tx)
		if err != nil {
			errs[i] = err
			v.counts = append(v.counts, 0)
			v.signers = append(v.signers, nil)
			continue
		}
		for j := range tx.Signatures {
			v.batch.Add((*[32]byte)(&signers[j]), msg, tx.Signatures[j][:])
		}
		v.counts = append(v.counts, len(tx.Signatures))
		v.signers = append(v.signers, signers)
	}

	if v.batch.Verify() {
		return
	}

	lane := 0
	for i, count := range v.counts {
		for j := 0; j < count; j++ {
			if !v.batch.OK(lane+j) && errs[i] == nil {
				errs[i] = fmt.Errorf("invalid signature by %s", v.signers[i][j])
			}
		}
		lane += count
	}
}

// prepare runs the checks that must precede verification and that determine a
// transaction's result on their own.
func prepare(tx *solana.Transaction) ([]solana.PublicKey, []byte, error) {
	msg, err := MessageBytes(tx)
	if err != nil {
		return nil, nil, err
	}
	signers := tx.Message.Signers()
	if len(signers) != len(tx.Signatures) {
		return nil, nil, fmt.Errorf("got %d signers, but %d signatures", len(signers), len(tx.Signatures))
	}
	return signers, msg, nil
}
