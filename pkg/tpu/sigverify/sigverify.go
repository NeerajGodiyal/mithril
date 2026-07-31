package sigverify

import (
	"errors"

	"github.com/Overclock-Validator/mithril/pkg/txverify"
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
//
// Prefer BatchVerifier: a transaction carries one or two signatures, so
// verifying packets one at a time leaves most of a vector group idle.
func VerifyPacket(data []byte) bool {
	tx, err := ParseTx(data)
	if err != nil {
		return false
	}
	return VerifyTransaction(tx)
}

func VerifyTransaction(tx *solana.Transaction) bool {
	if !admissible(tx) {
		return false
	}
	// Signature checking goes through txverify rather than being reimplemented
	// here. This path used to marshal the message itself and so omitted the
	// version-byte fixup, which meant a correctly signed versioned transaction
	// was dropped at ingest.
	return txverify.VerifyTransaction(tx) == nil
}

// admissible rejects shapes TPU must not admit regardless of cryptography: a
// transaction that requires no signatures at all, a signature list that
// disagrees with the header, or a header claiming more signers than the account
// table can supply.
func admissible(tx *solana.Transaction) bool {
	if tx == nil {
		return false
	}
	required := int(tx.Message.Header.NumRequiredSignatures)
	return required > 0 &&
		len(tx.Signatures) == required &&
		required <= len(tx.Message.AccountKeys)
}

// BatchVerifier parses and verifies many packets per call. It is reusable
// caller-owned scratch and is not safe for concurrent use; give each worker
// its own.
type BatchVerifier struct {
	inner txverify.BatchVerifier
	txs   []*solana.Transaction
	errs  []error
}

// Verify writes a verdict for each packet into ok, which must be at least as
// long as packets. A packet that fails to parse, or whose shape is
// inadmissible, is reported false without consuming a signature lane.
func (v *BatchVerifier) Verify(packets [][]byte, ok []bool) {
	clear(v.txs)
	v.txs = v.txs[:0]
	for _, data := range packets {
		tx, err := ParseTx(data)
		if err != nil || !admissible(tx) {
			// A nil entry keeps the packet's position so verdicts line up, and
			// the batch verifier reports it failed without adding lanes.
			v.txs = append(v.txs, nil)
			continue
		}
		v.txs = append(v.txs, tx)
	}

	if cap(v.errs) < len(v.txs) {
		v.errs = make([]error, len(v.txs))
	}
	v.errs = v.errs[:len(v.txs)]
	v.inner.Verify(v.txs, v.errs)

	for i := range v.txs {
		ok[i] = v.txs[i] != nil && v.errs[i] == nil
	}
}
