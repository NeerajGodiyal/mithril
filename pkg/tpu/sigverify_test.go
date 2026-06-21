package tpu

import (
	"testing"

	"github.com/gagliardetto/solana-go"
)

// maxPacketSignatures is the most required signatures that fit in a 1232-byte
// legacy TPU packet for a minimal transaction (no instructions).
const maxPacketSignatures = 12

func buildMaxSignerWireTx(t *testing.T) []byte {
	t.Helper()

	const n = maxPacketSignatures
	wallets := make([]*solana.Wallet, n)
	pubkeys := make(solana.PublicKeySlice, n)
	for i := range wallets {
		wallets[i] = solana.NewWallet()
		pubkeys[i] = wallets[i].PublicKey()
	}

	msg := solana.Message{
		Header: solana.MessageHeader{
			NumRequiredSignatures:       n,
			NumReadonlySignedAccounts:   0,
			NumReadonlyUnsignedAccounts: 0,
		},
		AccountKeys:     pubkeys,
		RecentBlockhash: solana.Hash{},
		Instructions:    []solana.CompiledInstruction{},
	}
	msgBytes, err := msg.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}

	tx := &solana.Transaction{
		Message:    msg,
		Signatures: make([]solana.Signature, n),
	}
	for i := range wallets {
		sig, err := wallets[i].PrivateKey.Sign(msgBytes)
		if err != nil {
			t.Fatalf("sign with key %d: %v", i, err)
		}
		tx.Signatures[i] = sig
	}

	wire, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal tx: %v", err)
	}
	if len(wire) > 1232 {
		t.Fatalf("wire tx len %d exceeds 1232-byte packet limit", len(wire))
	}
	if !VerifyPacket(wire) {
		t.Fatal("expected max-signer wire tx to verify")
	}
	return wire
}

func TestVerifyPacketMaxSignaturesExhaustive(t *testing.T) {
	validWire := buildMaxSignerWireTx(t)

	const n = maxPacketSignatures
	const allValidMask = 1<<n - 1

	for mask := 0; mask < 1<<n; mask++ {
		wire := append([]byte(nil), validWire...)
		for i := 0; i < n; i++ {
			if (mask>>i)&1 == 0 {
				wire[1+i*64] ^= 0xff
			}
		}

		want := mask == allValidMask
		if got := VerifyPacket(wire); got != want {
			t.Fatalf("mask=%#04x: got %v want %v", mask, got, want)
		}
	}
}
