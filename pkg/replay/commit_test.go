package replay

import (
	"math"
	"sync"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplySuccessfulTransactionCommitsTransfer(t *testing.T) {
	slotCtx, cleanup := newCommitTestSlotCtx()
	defer cleanup()

	wire := txfixture.MustSignedTransferWire(0)
	tx, err := solana.TransactionFromBytes(wire)
	require.NoError(t, err)

	output := LoadAndExecuteTransaction(LoadAndExecuteTransactionInput{
		SlotCtx:     slotCtx,
		Transaction: tx,
	})
	require.Nil(t, output.ProcessingResult.TransactionError)

	payerBefore, err := slotCtx.GetAccount(txfixture.PayerPubkey())
	require.NoError(t, err)
	destBefore, err := slotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)

	require.NoError(t, ApplySuccessfulTransaction(slotCtx, output))

	payerAfter, err := slotCtx.GetAccount(txfixture.PayerPubkey())
	require.NoError(t, err)
	destAfter, err := slotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)

	assert.Less(t, payerAfter.Lamports, payerBefore.Lamports)
	assert.Greater(t, destAfter.Lamports, destBefore.Lamports)
}

func TestApplySuccessfulTransactionRejectsFailedOutput(t *testing.T) {
	slotCtx, cleanup := newCommitTestSlotCtx()
	defer cleanup()

	output := sanitizeFailureOutput()
	err := ApplySuccessfulTransaction(slotCtx, output)
	require.Error(t, err)
}

func newCommitTestSlotCtx() (*sealevel.SlotCtx, func()) {
	feats := features.NewFeaturesDefault()
	feats.EnableFeature(features.FormalizeLoadedTransactionDataSize, 0)

	mem := accounts.NewMemAccounts()
	systemAcct := &accounts.Account{
		Key:        addresses.SystemProgramAddr,
		Lamports:   1,
		Owner:      addresses.NativeLoaderAddr,
		Executable: true,
		RentEpoch:  math.MaxUint64,
	}
	_ = mem.SetAccountWithoutLock(addresses.SystemProgramAddr, systemAcct)
	_ = mem.SetAccountWithoutLock(txfixture.PayerPubkey(), &accounts.Account{
		Key: txfixture.PayerPubkey(), Lamports: 10_000_000_000, Owner: addresses.SystemProgramAddr, RentEpoch: math.MaxUint64,
	})
	_ = mem.SetAccountWithoutLock(txfixture.DestPubkey(), &accounts.Account{
		Key: txfixture.DestPubkey(), Lamports: 10_000_000, Owner: addresses.SystemProgramAddr, RentEpoch: math.MaxUint64,
	})

	prevRBH := sealevel.SysvarCache.RecentBlockHashes.Sysvar
	rbh := sealevel.SysvarRecentBlockhashes{{Blockhash: txfixture.TestBlockhash(), FeeCalculator: sealevel.FeeCalculator{LamportsPerSignature: 5000}}}
	sealevel.SysvarCache.RecentBlockHashes.Sysvar = &rbh

	prevRent := sealevel.SysvarCache.Rent.Sysvar
	rent := sealevel.NewDefaultRentSysvar()
	sealevel.SysvarCache.Rent.Sysvar = &rent

	slotCtx := &sealevel.SlotCtx{
		Slot:            42,
		Features:        feats,
		Accounts:        mem,
		FeeRateGovernor: &sealevel.FeeRateGovernor{PrevLamportsPerSignature: 5000},
		LastBlockhash:   txfixture.TestBlockhash(),
		AcctMapsMu:      &sync.Mutex{},
		ModifiedAccts:   make(map[solana.PublicKey]bool),
		WritableAccts:   make(map[solana.PublicKey]bool),
	}
	return slotCtx, func() {
		sealevel.SysvarCache.RecentBlockHashes.Sysvar = prevRBH
		sealevel.SysvarCache.Rent.Sysvar = prevRent
	}
}
