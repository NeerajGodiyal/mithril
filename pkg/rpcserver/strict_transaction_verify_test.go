package rpcserver

import (
	"context"
	stded25519 "crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"filippo.io/edwards25519"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/txverify"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func forgedSmallOrderTransaction(t *testing.T) *solana.Transaction {
	t.Helper()
	var signer solana.PublicKey
	signer[0] = 1
	tx := &solana.Transaction{
		Signatures: []solana.Signature{{}},
		Message: solana.Message{
			Header:      solana.MessageHeader{NumRequiredSignatures: 1},
			AccountKeys: []solana.PublicKey{signer},
		},
	}
	message, err := txverify.MessageBytes(tx)
	require.NoError(t, err)
	uniform := make([]byte, 64)
	_, err = rand.Read(uniform)
	require.NoError(t, err)
	scalar, err := edwards25519.NewScalar().SetUniformBytes(uniform)
	require.NoError(t, err)
	r := (&edwards25519.Point{}).ScalarBaseMult(scalar)
	copy(tx.Signatures[0][:32], r.Bytes())
	copy(tx.Signatures[0][32:], scalar.Bytes())
	require.True(t, stded25519.Verify(signer[:], message, tx.Signatures[0][:]), "fixture must pass permissive stdlib verification")
	return tx
}

func TestRPCSignatureVerificationRejectsSmallOrderForgery(t *testing.T) {
	tx := forgedSmallOrderTransaction(t)
	wire, err := tx.MarshalBinary()
	require.NoError(t, err)
	bankhash := solana.Hash{7}
	server := &RpcServer{slotCtx: &sealevel.SlotCtx{
		Slot: 42, FinalBankhash: bankhash[:], Features: features.NewFeaturesDefault(),
	}}

	resp, err := server.SimulateTransaction(context.Background(), mustRawParams(t, []interface{}{
		base64.StdEncoding.EncodeToString(wire),
		map[string]interface{}{"encoding": "base64", "sigVerify": true},
	}))
	require.NoError(t, err)
	require.Equal(t, "SignatureFailure", resp.Value.Err)

	_, err = server.SendTransaction(context.Background(), mustRawParams(t, []interface{}{
		base64.StdEncoding.EncodeToString(wire),
		map[string]interface{}{"encoding": "base64"},
	}))
	var preflight *SendTransactionPreflightFailureError
	require.ErrorAs(t, err, &preflight)
	require.Equal(t, "SignatureFailure", preflight.Result.Err)
}
