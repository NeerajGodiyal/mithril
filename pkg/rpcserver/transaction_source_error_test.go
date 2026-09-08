package rpcserver

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

type failedAccountSource struct{ err error }

func (source failedAccountSource) GetAccount(uint64, solana.PublicKey) (*accounts.Account, error) {
	return nil, source.err
}

func TestTransactionRPCRejectsAccountSourceFailure(t *testing.T) {
	wire := txfixture.MustSignedTransferWire(1)
	tx, err := solana.TransactionFromBytes(wire)
	require.NoError(t, err)
	previousRecent := sealevel.SysvarCache.RecentBlockHashes.Sysvar
	recent := sealevel.SysvarRecentBlockhashes{{Blockhash: tx.Message.RecentBlockhash}}
	sealevel.SysvarCache.RecentBlockHashes.Sysvar = &recent
	t.Cleanup(func() { sealevel.SysvarCache.RecentBlockHashes.Sysvar = previousRecent })
	sourceErr := errors.New("account read failed")

	for _, relaxed := range []bool{false, true} {
		name := "strict payer validation"
		if relaxed {
			name = "relaxed payer validation"
		}
		t.Run(name, func(t *testing.T) {
			feats := features.NewFeaturesDefault()
			if relaxed {
				feats.EnableFeature(features.RelaxFeePayerConstraint, 0)
			}
			slotCtx := &sealevel.SlotCtx{
				Slot:         123,
				Features:     feats,
				Accounts:     accounts.NewMemAccounts(),
				UnrootedRead: failedAccountSource{err: sourceErr},
			}
			server := &RpcServer{slotCtx: slotCtx}
			err := server.preflightSendTransaction(context.Background(), tx, slotCtx)
			require.ErrorIs(t, err, sourceErr)
			require.ErrorContains(t, err, "transaction preflight unavailable")

			response, err := server.SimulateTransaction(context.Background(), mustRawParams(t, []interface{}{
				base64.StdEncoding.EncodeToString(wire),
				map[string]interface{}{"encoding": "base64"},
			}))
			require.ErrorIs(t, err, sourceErr)
			require.ErrorContains(t, err, "transaction simulation unavailable")
			require.Equal(t, SimulateTransactionResp{}, response)
		})
	}
}
