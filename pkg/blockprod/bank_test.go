package blockprod

import (
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/addresses"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/costmodel"
	"github.com/Overclock-Validator/mithril/pkg/replay"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/Overclock-Validator/mithril/pkg/turbine"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errTestAccountBankStale = errors.New("test account bank stale")
var errTestAccountSourceFailure = errors.New("test account source failure")

type failingAccountReadValidator struct {
	failAt      int
	validations int
}

type failingStableAccountSource struct{}

type singleAccountReader struct {
	key     solana.PublicKey
	account *accounts.Account
}

func (r singleAccountReader) GetAccount(_ uint64, key solana.PublicKey) (*accounts.Account, error) {
	if key != r.key || r.account == nil {
		return nil, accountsdb.ErrNoAccount
	}
	return r.account.Clone(), nil
}

func leaderLookupTableAccount(key solana.PublicKey, loaded ...solana.PublicKey) *accounts.Account {
	data := make([]byte, sealevel.AddressLookupTableMetaSize+len(loaded)*solana.PublicKeyLength)
	binary.LittleEndian.PutUint32(data, sealevel.AddressLookupTableProgramStateLookupTable)
	binary.LittleEndian.PutUint64(data[4:], math.MaxUint64)
	for i, address := range loaded {
		copy(data[sealevel.AddressLookupTableMetaSize+i*solana.PublicKeyLength:], address[:])
	}
	return &accounts.Account{Key: key, Lamports: 1, Owner: [32]byte(addresses.AddressLookupTableAddr), Data: data}
}

func (failingStableAccountSource) GetAccount(uint64, solana.PublicKey) (*accounts.Account, error) {
	return nil, errTestAccountSourceFailure
}

func (r *failingAccountReadValidator) GetAccount(uint64, solana.PublicKey) (*accounts.Account, error) {
	return nil, accountsdb.ErrNoAccount
}

func (r *failingAccountReadValidator) ValidateAccountRead() error {
	r.validations++
	if r.validations >= r.failAt {
		return errTestAccountBankStale
	}
	return nil
}

type captureSink struct {
	batches [][]turbine.Entry
	bytes   []int
}

func (s *captureSink) OnEntryBatch(entries []turbine.Entry, batchBytes int) {
	s.batches = append(s.batches, append([]turbine.Entry(nil), entries...))
	s.bytes = append(s.bytes, batchBytes)
}

func setPayerLamports(t *testing.T, env *TestEnv, lamports uint64) {
	t.Helper()
	acct, err := env.SlotCtx.GetAccount(txfixture.PayerPubkey())
	require.NoError(t, err)
	acct.Lamports = lamports
	require.NoError(t, env.SlotCtx.SetAccount(txfixture.PayerPubkey(), acct))
}

func TestWorkingBankDropsPayerThatCannotRemainRentExempt(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()

	rent := sealevel.NewDefaultRentSysvar()
	rentMin := rent.MinimumBalance(0)
	require.Equal(t, uint64(890880), rentMin)
	setPayerLamports(t, env, rentMin+5000-1)
	payerBefore, err := env.SlotCtx.GetAccount(txfixture.PayerPubkey())
	require.NoError(t, err)

	destBefore, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	result, reason := env.Bank.Forge(txfixture.MustSignedTransferWire(0))
	require.Equal(t, ForgeDroppedExecution, result)
	require.Equal(t, costmodel.ExceedNone, reason)

	destAfter, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	payerAfter, err := env.SlotCtx.GetAccount(txfixture.PayerPubkey())
	require.NoError(t, err)
	assert.Equal(t, payerBefore.Lamports, payerAfter.Lamports)
	assert.Equal(t, destBefore.Lamports, destAfter.Lamports)
	assert.Empty(t, env.Bank.ForgedTransactions())
	assert.Zero(t, env.Bank.CostTracker().BlockCost())
	assert.Zero(t, env.Bank.EntryBuilder().PendingCount())
	assert.Zero(t, env.Bank.TxFeeAccumulator().TotalFees)
}

func TestWorkingBankDropsPreExecutionInstructionErrorWithoutStopping(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()
	payer := txfixture.PayerPubkey()
	limit := []byte{sealevel.ComputeBudgetInstrTypeSetComputeUnitLimit, 1, 0, 0, 0}
	tx, err := solana.NewTransaction([]solana.Instruction{
		solana.NewInstruction(addresses.ComputeBudgetProgramAddr, nil, limit),
		solana.NewInstruction(addresses.ComputeBudgetProgramAddr, nil, limit),
	}, txfixture.TestBlockhash(), solana.TransactionPayer(payer))
	require.NoError(t, err)
	privateKey := txfixture.PayerPrivateKey()
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == payer {
			return &privateKey
		}
		return nil
	})
	require.NoError(t, err)
	wire, err := tx.MarshalBinary()
	require.NoError(t, err)

	result, reason := env.Bank.Forge(wire)
	require.Equal(t, ForgeDroppedExecution, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	require.True(t, env.Bank.accepting)
	require.Empty(t, env.Bank.ForgedTransactions())
	require.Zero(t, env.Bank.EntryBuilder().PendingCount())
}

func TestWorkingBankForgesV0LookupTransactionAndPreservesWire(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()
	payer := txfixture.PayerPubkey()
	destination := txfixture.DestPubkey()
	tableKey := solana.PublicKey{0xa1, 1}
	env.SlotCtx.UnrootedRead = singleAccountReader{
		key: tableKey, account: leaderLookupTableAccount(tableKey, destination),
	}
	recent := sealevel.SysvarRecentBlockhashes{{
		Blockhash: txfixture.TestBlockhash(), FeeCalculator: sealevel.FeeCalculator{LamportsPerSignature: 5000},
	}}
	rent := sealevel.NewDefaultRentSysvar()
	slotHashes := sealevel.SysvarSlotHashes{}
	bankSysvars, err := sealevel.NewBankSysvars(env.SlotCtx.Slot,
		&accounts.Account{Key: sealevel.SysvarRecentBlockHashesAddr, Lamports: 1, Data: recent.MustMarshal()},
		&accounts.Account{Key: sealevel.SysvarRentAddr, Lamports: 1, Data: rent.MustMarshal()},
		&accounts.Account{Key: sealevel.SysvarSlotHashesAddr, Lamports: 1, Data: (&slotHashes).MustMarshal()},
	)
	require.NoError(t, err)
	require.NoError(t, env.SlotCtx.PublishBankSysvars(bankSysvars))

	tx, err := solana.NewTransaction(
		[]solana.Instruction{system.NewTransferInstruction(1, payer, destination).Build()},
		txfixture.TestBlockhash(),
		solana.TransactionPayer(payer),
		solana.TransactionAddressTables(map[solana.PublicKey]solana.PublicKeySlice{tableKey: {destination}}),
	)
	require.NoError(t, err)
	require.True(t, tx.Message.IsVersioned())
	require.Equal(t, 1, tx.Message.AddressTableLookups.NumLookups())
	privateKey := txfixture.PayerPrivateKey()
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == payer {
			return &privateKey
		}
		return nil
	})
	require.NoError(t, err)
	wire, err := tx.MarshalBinary()
	require.NoError(t, err)
	destinationBefore, err := env.SlotCtx.GetAccount(destination)
	require.NoError(t, err)

	result, reason := env.Bank.Forge(wire)
	require.Equal(t, ForgeAccepted, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	destinationAfter, err := env.SlotCtx.GetAccount(destination)
	require.NoError(t, err)
	require.Equal(t, destinationBefore.Lamports+1, destinationAfter.Lamports)
	forged := env.Bank.ForgedTransactions()
	require.Len(t, forged, 1)
	preservedWire, err := forged[0].MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, wire, preservedWire)
}

func TestWorkingBankStopsWhenPayerWouldFallBelowRent(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()

	rent := sealevel.NewDefaultRentSysvar()
	rentMin := rent.MinimumBalance(0)
	// One 1-lamport transfer plus its 5000-lamport fee, leaving exactly rent-exempt.
	setPayerLamports(t, env, rentMin+5000+1)

	destBefore, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	result, reason := env.Bank.Forge(txfixture.MustSignedTransferWire(0))
	require.Equal(t, ForgeAccepted, result)
	require.Equal(t, costmodel.ExceedNone, reason)

	result, reason = env.Bank.Forge(txfixture.MustSignedTransferWire(1))
	require.Equal(t, ForgeDroppedExecution, result)
	require.Equal(t, costmodel.ExceedNone, reason)

	destAfter, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	assert.Equal(t, destBefore.Lamports+1, destAfter.Lamports)
	assert.Len(t, env.Bank.ForgedTransactions(), 1)
}

func TestWorkingBankIncludesExecutedFailureWithFeeRollback(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{CaptureRootedEvents: true})
	defer env.Close()
	payer := txfixture.PayerPubkey()
	destination := txfixture.DestPubkey()
	payerBefore, err := env.SlotCtx.GetAccount(payer)
	require.NoError(t, err)
	destinationBefore, err := env.SlotCtx.GetAccount(destination)
	require.NoError(t, err)

	transfer := system.NewTransferInstruction(100, payer, destination).Build()
	fail := solana.NewInstruction(addresses.SystemProgramAddr, nil, []byte{0xff, 0xff, 0xff, 0xff})
	tx, err := solana.NewTransaction(
		[]solana.Instruction{transfer, fail}, txfixture.TestBlockhash(), solana.TransactionPayer(payer),
	)
	require.NoError(t, err)
	privateKey := txfixture.PayerPrivateKey()
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == payer {
			return &privateKey
		}
		return nil
	})
	require.NoError(t, err)
	wire, err := tx.MarshalBinary()
	require.NoError(t, err)

	result, reason := env.Bank.Forge(wire)
	require.Equal(t, ForgeAccepted, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	require.Len(t, env.Bank.ForgedTransactions(), 1)
	require.Equal(t, 1, env.Bank.EntryBuilder().PendingCount())
	payerAfter, err := env.SlotCtx.GetAccount(payer)
	require.NoError(t, err)
	destinationAfter, err := env.SlotCtx.GetAccount(destination)
	require.NoError(t, err)
	require.Equal(t, payerBefore.Lamports-5_000, payerAfter.Lamports)
	require.Equal(t, destinationBefore.Lamports, destinationAfter.Lamports)

	observations, captured := env.Bank.RootedEventObservations()
	require.True(t, captured)
	require.Len(t, observations, 1)
	require.False(t, observations[0].Succeeded)
	require.Equal(t, "InstructionError(1, InvalidInstructionData)", observations[0].Failure)
}

func TestWorkingBankCapturesLightweightTransactionOutcomes(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{CaptureOutcomes: true})
	defer env.Close()
	payer := txfixture.PayerPubkey()
	fail := solana.NewInstruction(addresses.SystemProgramAddr, nil, []byte{0xff, 0xff, 0xff, 0xff})
	tx, err := solana.NewTransaction([]solana.Instruction{fail}, txfixture.TestBlockhash(), solana.TransactionPayer(payer))
	require.NoError(t, err)
	privateKey := txfixture.PayerPrivateKey()
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == payer {
			return &privateKey
		}
		return nil
	})
	require.NoError(t, err)
	wire, err := tx.MarshalBinary()
	require.NoError(t, err)

	result, reason := env.Bank.Forge(wire)
	require.Equal(t, ForgeAccepted, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	require.Equal(t, []string{"InstructionError(0, InvalidInstructionData)"}, env.Bank.TransactionOutcomes())
	observations, captured := env.Bank.RootedEventObservations()
	require.False(t, captured)
	require.Empty(t, observations)
}

func TestWorkingBankIncludesFeesOnlyProgramLoadFailure(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{CaptureRootedEvents: true})
	defer env.Close()
	payer := txfixture.PayerPubkey()
	payerBefore, err := env.SlotCtx.GetAccount(payer)
	require.NoError(t, err)
	missingProgram := solana.NewWallet().PublicKey()
	tx, err := solana.NewTransaction(
		[]solana.Instruction{solana.NewInstruction(missingProgram, nil, nil)},
		txfixture.TestBlockhash(),
		solana.TransactionPayer(payer),
	)
	require.NoError(t, err)
	privateKey := txfixture.PayerPrivateKey()
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == payer {
			return &privateKey
		}
		return nil
	})
	require.NoError(t, err)
	wire, err := tx.MarshalBinary()
	require.NoError(t, err)

	result, reason := env.Bank.Forge(wire)
	require.Equal(t, ForgeAccepted, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	require.Len(t, env.Bank.ForgedTransactions(), 1)
	require.Equal(t, 1, env.Bank.EntryBuilder().PendingCount())
	payerAfter, err := env.SlotCtx.GetAccount(payer)
	require.NoError(t, err)
	require.Equal(t, payerBefore.Lamports-5_000, payerAfter.Lamports)

	observations, captured := env.Bank.RootedEventObservations()
	require.True(t, captured)
	require.Len(t, observations, 1)
	require.False(t, observations[0].Succeeded)
	require.Equal(t, replay.TransactionErrorProgramAccountNotFound.String(), observations[0].Failure)
}

func TestWorkingBankDoesNotPublishAfterAccountBankChanges(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()
	validator := &failingAccountReadValidator{failAt: 3}
	env.SlotCtx.UnrootedRead = validator

	result, reason := env.Bank.Forge(txfixture.MustSignedTransferWire(0))
	require.Equal(t, ForgeDroppedNoLeader, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	require.GreaterOrEqual(t, validator.validations, validator.failAt)
	require.Empty(t, env.Bank.ForgedTransactions())
	require.Zero(t, env.Bank.EntryBuilder().PendingCount())
	require.False(t, env.Bank.accepting)
}

func TestWorkingBankStopsOnFeePayerSourceFailure(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()
	env.SlotCtx.Accounts = accounts.NewMemAccounts()
	env.SlotCtx.UnrootedRead = failingStableAccountSource{}

	result, reason := env.Bank.Forge(txfixture.MustSignedTransferWire(0))
	require.Equal(t, ForgeDroppedNoLeader, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	require.False(t, env.Bank.accepting)
	require.Empty(t, env.Bank.ForgedTransactions())
}

func TestClassifyBufferedUsesBankLocalRecentBlockhashes(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()
	globalOnly := solana.Hash{0x44}
	globalRecent := sealevel.SysvarRecentBlockhashes{{Blockhash: globalOnly}}
	sealevel.SysvarCache.RecentBlockHashes.Sysvar = &globalRecent

	require.Equal(t, BufferedKeep, env.Bank.ClassifyBuffered(&solana.Transaction{Message: solana.Message{RecentBlockhash: txfixture.TestBlockhash()}}, [32]byte{}))
	require.Equal(t, BufferedExpired, env.Bank.ClassifyBuffered(&solana.Transaction{Message: solana.Message{RecentBlockhash: globalOnly}}, [32]byte{}))

	bankRecent := sealevel.SysvarRecentBlockhashes{{Blockhash: globalOnly}}
	env.SlotCtx.LatestEvictedBlockhash = solana.Hash{0x33}
	bankSysvars, err := sealevel.NewBankSysvars(env.SlotCtx.Slot, &accounts.Account{
		Key: sealevel.SysvarRecentBlockHashesAddr, Lamports: 1, Data: bankRecent.MustMarshal(),
	})
	require.NoError(t, err)
	require.NoError(t, env.SlotCtx.PublishBankSysvars(bankSysvars))
	require.Equal(t, BufferedExpired, env.Bank.ClassifyBuffered(&solana.Transaction{Message: solana.Message{RecentBlockhash: txfixture.TestBlockhash()}}, [32]byte{}))
	require.Equal(t, BufferedKeep, env.Bank.ClassifyBuffered(&solana.Transaction{Message: solana.Message{RecentBlockhash: globalOnly}}, [32]byte{}))

	latestEvicted := solana.Hash{0x55}
	env.SlotCtx.LatestEvictedBlockhash = latestEvicted
	require.Equal(t, BufferedKeep, env.Bank.ClassifyBuffered(&solana.Transaction{Message: solana.Message{RecentBlockhash: latestEvicted}}, [32]byte{}))

	nonceData := make([]byte, 4)
	binary.LittleEndian.PutUint32(nonceData, sealevel.SystemProgramInstrTypeAdvanceNonceAccount)
	potentialNonce := &solana.Transaction{Message: solana.Message{
		RecentBlockhash: solana.Hash{0x66},
		AccountKeys:     []solana.PublicKey{addresses.SystemProgramAddr},
		Instructions: []solana.CompiledInstruction{{
			ProgramIDIndex: 0,
			Data:           nonceData,
		}},
	}}
	require.Equal(t, BufferedKeep, env.Bank.ClassifyBuffered(potentialNonce, [32]byte{}))
}

func mustSignedTransfer(t *testing.T, lamports uint64) *solana.Transaction {
	t.Helper()
	tx, err := solana.NewTransaction(
		[]solana.Instruction{
			system.NewTransferInstruction(lamports, txfixture.PayerPubkey(), txfixture.DestPubkey()).Build(),
		},
		txfixture.TestBlockhash(),
		solana.TransactionPayer(txfixture.PayerPubkey()),
	)
	require.NoError(t, err)
	payerKey := txfixture.PayerPrivateKey()
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(txfixture.PayerPubkey()) {
			return &payerKey
		}
		return nil
	})
	require.NoError(t, err)
	return tx
}

func TestWorkingBankAcceptsTransferThatDrainsPayerToZero(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()

	const transfer = uint64(1_000_000)
	setPayerLamports(t, env, 5000+transfer)

	destBefore, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	tx := mustSignedTransfer(t, transfer)
	result, reason := env.Bank.ForgeTransaction(tx, 200)
	require.Equal(t, ForgeAccepted, result)
	require.Equal(t, costmodel.ExceedNone, reason)

	payerAfter, err := env.SlotCtx.GetAccount(txfixture.PayerPubkey())
	require.NoError(t, err)
	destAfter, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	assert.Zero(t, payerAfter.Lamports)
	assert.Equal(t, destBefore.Lamports+transfer, destAfter.Lamports)
}

func TestWorkingBankIncludesRentFailureAndRollsBackTransfer(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()

	const leftover = uint64(100)
	const transfer = uint64(1_000_000)
	setPayerLamports(t, env, 5000+transfer+leftover)

	destBefore, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	tx := mustSignedTransfer(t, transfer)
	result, reason := env.Bank.ForgeTransaction(tx, 200)
	require.Equal(t, ForgeAccepted, result)
	require.Equal(t, costmodel.ExceedNone, reason)

	payerAfter, err := env.SlotCtx.GetAccount(txfixture.PayerPubkey())
	require.NoError(t, err)
	destAfter, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	assert.Equal(t, transfer+leftover, payerAfter.Lamports)
	assert.Equal(t, destBefore.Lamports, destAfter.Lamports)
	assert.Len(t, env.Bank.ForgedTransactions(), 1)
	assert.Equal(t, 1, env.Bank.EntryBuilder().PendingCount())
	assert.Equal(t, uint64(5_000), env.Bank.TxFeeAccumulator().TotalFees)
}

func TestWorkingBankForgesTransfer(t *testing.T) {
	sink := &captureSink{}
	env := NewTestEnv(TestEnvConfig{Sink: sink})
	defer env.Close()

	wire := txfixture.MustSignedTransferWire(0)
	result, reason := env.Bank.Forge(wire)
	assert.Equal(t, ForgeAccepted, result)
	assert.Equal(t, costmodel.ExceedNone, reason)
	assert.Equal(t, 1, env.Bank.EntryBuilder().PendingCount())
}

func TestWorkingBankCapturesOnlyAcceptedRootedTransactions(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{CaptureRootedEvents: true})
	defer env.Close()

	wire := txfixture.MustSignedTransferWire(0)
	result, reason := env.Bank.Forge(wire)
	require.Equal(t, ForgeAccepted, result)
	require.Equal(t, costmodel.ExceedNone, reason)

	observations, captured := env.Bank.RootedEventObservations()
	require.True(t, captured)
	require.Len(t, observations, 1)
	require.True(t, observations[0].Succeeded)
	require.Equal(t, uint32(0), observations[0].Index)
	require.NotEmpty(t, observations[0].Message)
	require.NotEmpty(t, observations[0].AccountKeys)

	result, reason = env.Bank.Forge(wire)
	require.Equal(t, ForgeDroppedAlreadyProcessed, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	afterDrop, _ := env.Bank.RootedEventObservations()
	require.Len(t, afterDrop, 1, "rejected transactions must not enter the rooted stream")

	observations[0].Message[0] ^= 0xff
	owned, _ := env.Bank.RootedEventObservations()
	require.NotEqual(t, observations[0].Message, owned[0].Message)
}

func TestWorkingBankSkipsRootedCaptureWhenDisabled(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()

	result, _ := env.Bank.Forge(txfixture.MustSignedTransferWire(0))
	require.Equal(t, ForgeAccepted, result)
	observations, captured := env.Bank.RootedEventObservations()
	require.False(t, captured)
	require.Empty(t, observations)
}

func TestWorkingBankRejectsExactDuplicateMessage(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()

	wire := txfixture.MustSignedTransferWire(0)
	result, reason := env.Bank.Forge(wire)
	require.Equal(t, ForgeAccepted, result)
	require.Equal(t, costmodel.ExceedNone, reason)

	destAfterFirst, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	costAfterFirst := env.Bank.CostTracker().BlockCost()

	result, reason = env.Bank.Forge(wire)
	require.Equal(t, ForgeDroppedAlreadyProcessed, result)
	require.Equal(t, costmodel.ExceedNone, reason)

	destAfterDuplicate, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	assert.Equal(t, destAfterFirst.Lamports, destAfterDuplicate.Lamports)
	assert.Equal(t, costAfterFirst, env.Bank.CostTracker().BlockCost())
	assert.Len(t, env.Bank.ForgedTransactions(), 1)
	assert.Equal(t, uint64(1), env.Bank.NumSignatures())
	assert.Equal(t, 1, env.Bank.EntryBuilder().PendingCount())
}

func TestWorkingBankRejectsExactAncestorMessageBeforeExecution(t *testing.T) {
	tx := mustBankTestTransaction(t, txfixture.MustSignedTransferWire(0))
	statuses := mustAncestorTransactionStatusView(t, tx)
	env := NewTestEnv(TestEnvConfig{TransactionStatuses: statuses})
	defer env.Close()

	destBefore, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	result, reason := env.Bank.ForgeTransaction(tx, len(txfixture.MustSignedTransferWire(0)))
	require.Equal(t, ForgeDroppedAlreadyProcessed, result)
	require.Equal(t, costmodel.ExceedNone, reason)

	destAfter, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	assert.Equal(t, destBefore.Lamports, destAfter.Lamports)
	assert.Zero(t, env.Bank.CostTracker().BlockCost())
	assert.Empty(t, env.Bank.ForgedTransactions())
	assert.Zero(t, env.Bank.EntryBuilder().PendingCount())
}

func TestWorkingBankMissingAncestorViewFailsClosed(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()
	env.Bank.ancestorStatuses = nil

	result, reason := env.Bank.Forge(txfixture.MustSignedTransferWire(0))
	require.Equal(t, ForgeDroppedNoLeader, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	assert.Zero(t, env.Bank.CostTracker().BlockCost())
	assert.Empty(t, env.Bank.ForgedTransactions())
}

func TestWorkingBankIncompleteAncestorViewFailsClosed(t *testing.T) {
	cache, err := replay.NewTransactionStatusCacheFromSnapshot(nil)
	require.NoError(t, err)
	env := NewTestEnv(TestEnvConfig{TransactionStatuses: cache.View()})
	defer env.Close()

	result, reason := env.Bank.Forge(txfixture.MustSignedTransferWire(0))
	require.Equal(t, ForgeDroppedNoLeader, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	assert.Zero(t, env.Bank.CostTracker().BlockCost())
	assert.Empty(t, env.Bank.ForgedTransactions())
}

func TestWorkingBankRejectsResignedAncestorMessage(t *testing.T) {
	wire := txfixture.MustSignedTransferWire(0)
	ancestor := mustBankTestTransaction(t, wire)
	statuses := mustAncestorTransactionStatusView(t, ancestor)
	env := NewTestEnv(TestEnvConfig{TransactionStatuses: statuses})
	defer env.Close()

	retry := *ancestor
	retry.Signatures = append([]solana.Signature(nil), ancestor.Signatures...)
	retry.Signatures[0][0] ^= 0xff
	result, reason := env.Bank.ForgeTransaction(&retry, len(wire))
	require.Equal(t, ForgeDroppedAlreadyProcessed, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	assert.Zero(t, env.Bank.CostTracker().BlockCost())
	assert.Empty(t, env.Bank.ForgedTransactions())
}

func TestWorkingBankAllowsAncestorPayloadWithDifferentRecentBlockhash(t *testing.T) {
	wire := txfixture.MustSignedTransferWire(0)
	producerTx := mustBankTestTransaction(t, wire)
	ancestor := *producerTx
	ancestor.Message.RecentBlockhash = solana.Hash{0x42}
	statuses := mustAncestorTransactionStatusView(t, &ancestor)
	env := NewTestEnv(TestEnvConfig{TransactionStatuses: statuses})
	defer env.Close()

	result, reason := env.Bank.ForgeTransaction(producerTx, len(wire))
	require.Equal(t, ForgeAccepted, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	assert.NotZero(t, env.Bank.CostTracker().BlockCost())
	assert.Len(t, env.Bank.ForgedTransactions(), 1)
}

func TestWorkingBankPinnedAncestorViewSurvivesConcurrentReplayUnwind(t *testing.T) {
	wire := txfixture.MustSignedTransferWire(0)
	ancestor := mustBankTestTransaction(t, wire)
	cache := replay.NewTransactionStatusCache()
	require.NoError(t, cache.CommitBlock(bankTestStatusBlock(41, ancestor)))
	pinned := cache.View()
	env := NewTestEnv(TestEnvConfig{TransactionStatuses: pinned})
	defer env.Close()

	replacement := mustBankTestTransaction(t, txfixture.MustSignedTransferWire(1))
	start := make(chan struct{})
	updateErr := make(chan error, 1)
	go func() {
		<-start
		for range 64 {
			cache.Unwind(41)
			if err := cache.CommitBlock(bankTestStatusBlock(41, replacement)); err != nil {
				updateErr <- err
				return
			}
		}
		updateErr <- nil
	}()

	close(start)
	retry := *ancestor
	retry.Signatures = append([]solana.Signature(nil), ancestor.Signatures...)
	retry.Signatures[0][0] ^= 0xff
	result, reason := env.Bank.ForgeTransaction(&retry, len(wire))
	require.NoError(t, <-updateErr)
	require.Equal(t, ForgeDroppedAlreadyProcessed, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	assert.Zero(t, env.Bank.CostTracker().BlockCost())
	assert.Empty(t, env.Bank.ForgedTransactions())

	contains, err := cache.View().ContainsTransaction(ancestor)
	require.NoError(t, err)
	assert.False(t, contains, "the replay cache should now expose the replacement branch")
}

func mustBankTestTransaction(t *testing.T, wire []byte) *solana.Transaction {
	t.Helper()
	tx, err := solana.TransactionFromBytes(wire)
	require.NoError(t, err)
	return tx
}

func mustAncestorTransactionStatusView(t *testing.T, tx *solana.Transaction) *replay.TransactionStatusView {
	t.Helper()
	cache := replay.NewTransactionStatusCache()
	require.NoError(t, cache.CommitBlock(bankTestStatusBlock(41, tx)))
	return cache.View()
}

func bankTestStatusBlock(slot uint64, txs ...*solana.Transaction) *b.Block {
	return &b.Block{Slot: slot, ParentSlot: slot - 1, Transactions: txs}
}

func TestWorkingBankRejectsSameMessageWithDifferentSignature(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()

	wire := txfixture.MustSignedTransferWire(0)
	tx, err := solana.TransactionFromBytes(wire)
	require.NoError(t, err)
	require.NotEmpty(t, tx.Signatures)

	result, _ := env.Bank.ForgeTransaction(tx, len(wire))
	require.Equal(t, ForgeAccepted, result)
	destAfterFirst, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)

	duplicate := *tx
	duplicate.Signatures = append([]solana.Signature(nil), tx.Signatures...)
	duplicate.Signatures[0][0] ^= 0xff
	result, reason := env.Bank.ForgeTransaction(&duplicate, len(wire))
	require.Equal(t, ForgeDroppedAlreadyProcessed, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	destAfterDuplicate, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	assert.Equal(t, destAfterFirst.Lamports, destAfterDuplicate.Lamports)
	assert.Len(t, env.Bank.ForgedTransactions(), 1)
	assert.Equal(t, uint64(1), env.Bank.NumSignatures())
	assert.Equal(t, 1, env.Bank.EntryBuilder().PendingCount())
}

func TestWorkingBankConcurrentDuplicateMessageCommitsOnce(t *testing.T) {
	sink := &captureSink{}
	env := NewTestEnv(TestEnvConfig{Sink: sink})
	defer env.Close()

	wire := txfixture.MustSignedTransferWire(0)
	tx, err := solana.TransactionFromBytes(wire)
	require.NoError(t, err)
	expectedCost, err := costmodel.EstimateTransactionCost(tx, env.SlotCtx.Features)
	require.NoError(t, err)

	destBefore, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	destLamportsBefore := destBefore.Lamports
	payerBefore, err := env.SlotCtx.GetAccount(txfixture.PayerPubkey())
	require.NoError(t, err)
	payerLamportsBefore := payerBefore.Lamports

	const submissions = 64
	type outcome struct {
		result ForgeResult
		reason costmodel.ExceedReason
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, submissions)
	var wg sync.WaitGroup
	wg.Add(submissions)
	for range submissions {
		go func() {
			defer wg.Done()
			<-start
			result, reason := env.Bank.Forge(wire)
			outcomes <- outcome{result: result, reason: reason}
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)

	accepted := 0
	alreadyProcessed := 0
	for got := range outcomes {
		require.Equal(t, costmodel.ExceedNone, got.reason)
		switch got.result {
		case ForgeAccepted:
			accepted++
		case ForgeDroppedAlreadyProcessed:
			alreadyProcessed++
		default:
			t.Fatalf("unexpected forge result: %s", got.result)
		}
	}
	require.Equal(t, 1, accepted)
	require.Equal(t, submissions-1, alreadyProcessed)

	destAfter, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	assert.Equal(t, destLamportsBefore+1, destAfter.Lamports)
	payerAfter, err := env.SlotCtx.GetAccount(txfixture.PayerPubkey())
	require.NoError(t, err)
	assert.Equal(t, payerLamportsBefore-5_001, payerAfter.Lamports)
	assert.Less(t, env.Bank.CostTracker().BlockCost(), expectedCost.Sum())
	assert.Greater(t, env.Bank.CostTracker().BlockCost(), expectedCost.SignatureCost)
	feeInfo := env.Bank.TxFeeAccumulator()
	assert.Equal(t, uint64(5_000), feeInfo.ExecutionFees)
	assert.Zero(t, feeInfo.PriorityFees)
	assert.Equal(t, uint64(5_000), feeInfo.TotalFees)
	assert.Len(t, env.Bank.ForgedTransactions(), 1)
	assert.Equal(t, uint64(1), env.Bank.NumSignatures())
	assert.Equal(t, 1, env.Bank.EntryBuilder().PendingCount())
	assert.Empty(t, sink.batches)

	env.Bank.Freeze()
	assert.Equal(t, 0, env.Bank.EntryBuilder().PendingCount())
	require.Len(t, sink.batches, 1)
	require.Len(t, sink.batches[0], 1)
	assert.Len(t, sink.batches[0][0].Txns, 1)
	assert.Greater(t, sink.bytes[0], 0)
}

func TestWorkingBankLifecycleWinsOverAlreadyProcessed(t *testing.T) {
	tests := []struct {
		name string
		stop func(*WorkingBank)
	}{
		{name: "freeze", stop: (*WorkingBank).Freeze},
		{name: "close", stop: (*WorkingBank).Close},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := NewTestEnv(TestEnvConfig{})
			defer env.Close()

			wire := txfixture.MustSignedTransferWire(0)
			result, reason := env.Bank.Forge(wire)
			require.Equal(t, ForgeAccepted, result)
			require.Equal(t, costmodel.ExceedNone, reason)

			destAfterFirst, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
			require.NoError(t, err)
			destLamportsAfterFirst := destAfterFirst.Lamports
			costAfterFirst := env.Bank.CostTracker().BlockCost()
			feesAfterFirst := env.Bank.TxFeeAccumulator()

			tt.stop(env.Bank)
			result, reason = env.Bank.Forge(wire)
			require.Equal(t, ForgeDroppedNoLeader, result)
			require.Equal(t, costmodel.ExceedNone, reason)

			destAfterDuplicate, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
			require.NoError(t, err)
			assert.Equal(t, destLamportsAfterFirst, destAfterDuplicate.Lamports)
			assert.Equal(t, costAfterFirst, env.Bank.CostTracker().BlockCost())
			assert.Equal(t, feesAfterFirst, env.Bank.TxFeeAccumulator())
			assert.Len(t, env.Bank.ForgedTransactions(), 1)
			assert.Equal(t, uint64(1), env.Bank.NumSignatures())
		})
	}
}

func TestWorkingBankDroppedTransactionDoesNotPoisonMessageHash(t *testing.T) {
	limits := costmodel.DefaultLimits()
	limits.BlockCost = 1
	env := NewTestEnv(TestEnvConfig{Limits: limits})
	defer env.Close()

	wire := txfixture.MustSignedTransferWire(0)
	destBefore, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	destLamportsBefore := destBefore.Lamports

	result, reason := env.Bank.Forge(wire)
	require.Equal(t, ForgeDroppedCost, result)
	require.Equal(t, costmodel.ExceedBlockCost, reason)
	assert.Zero(t, env.Bank.CostTracker().BlockCost())
	feeInfo := env.Bank.TxFeeAccumulator()
	assert.Zero(t, feeInfo.ExecutionFees)
	assert.Zero(t, feeInfo.PriorityFees)
	assert.Zero(t, feeInfo.TotalFees)
	assert.Empty(t, env.Bank.ForgedTransactions())
	assert.Zero(t, env.Bank.EntryBuilder().PendingCount())
	destAfterDrop, err := env.SlotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	assert.Equal(t, destLamportsBefore, destAfterDrop.Lamports)

	// Relax the test-only tracker after proving the first admission was dropped.
	// Retrying the identical message must still be eligible for execution.
	env.Bank.mu.Lock()
	env.Bank.costs = costmodel.NewCostTracker(costmodel.DefaultLimits())
	env.Bank.mu.Unlock()
	result, reason = env.Bank.Forge(wire)
	require.Equal(t, ForgeAccepted, result)
	require.Equal(t, costmodel.ExceedNone, reason)

	result, reason = env.Bank.Forge(wire)
	require.Equal(t, ForgeDroppedAlreadyProcessed, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	assert.Len(t, env.Bank.ForgedTransactions(), 1)
	assert.Equal(t, uint64(1), env.Bank.NumSignatures())
	assert.Equal(t, 1, env.Bank.EntryBuilder().PendingCount())
}

func TestWorkingBankDropsInvalidWire(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()

	result, _ := env.Bank.Forge([]byte{0x00})
	assert.Equal(t, ForgeDroppedParse, result)
}

func TestWorkingBankRejectsStalePacketAfterFreeze(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()

	env.Bank.Freeze()
	result, reason := env.Bank.Forge(txfixture.MustSignedTransferWire(0))
	assert.Equal(t, ForgeDroppedNoLeader, result)
	assert.Equal(t, costmodel.ExceedNone, reason)
	assert.Empty(t, env.Bank.ForgedTransactions())
}

func TestWorkingBankRebatesUnusedLoadedAccountsCost(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()

	wire := txfixture.MustSignedTransferWire(0)
	tx, err := solana.TransactionFromBytes(wire)
	require.NoError(t, err)
	estimated, err := costmodel.EstimateTransactionCost(tx, env.SlotCtx.Features)
	require.NoError(t, err)
	require.Equal(t, uint64(16_384), estimated.LoadedAccountsDataSizeCost)

	result, reason := env.Bank.Forge(wire)
	require.Equal(t, ForgeAccepted, result)
	require.Equal(t, costmodel.ExceedNone, reason)

	got := env.Bank.CostTracker().BlockCost()
	assert.Less(t, got, estimated.Sum())
	assert.Less(t, got, estimated.LoadedAccountsDataSizeCost)
	assert.Greater(t, got, estimated.SignatureCost)
}

func TestActualExecutionUsageUsesLoaderAccumulator(t *testing.T) {
	const loadedSize = uint32(8_248)
	_, got := actualExecutionUsage(replay.LoadAndExecuteTransactionOutput{
		LoadedAccountsDataSize: loadedSize,
	})
	assert.Equal(t, costmodel.LoadedAccountsDataSizeCost(loadedSize), got)
}

func TestWorkingBankDropsWhenBlockCostExceeded(t *testing.T) {
	limits := costmodel.DefaultLimits()
	limits.BlockCost = 1
	env := NewTestEnv(TestEnvConfig{Limits: limits})
	defer env.Close()

	wire := txfixture.MustSignedTransferWire(0)
	result, reason := env.Bank.Forge(wire)
	assert.Equal(t, ForgeDroppedCost, result)
	assert.Equal(t, costmodel.ExceedBlockCost, reason)
}

func TestPrepareScheduleClosesFullFECBatch(t *testing.T) {
	sink := &captureSink{}
	limits := costmodel.DefaultLimits()
	limits.MaxBatchBytes = 300
	env := NewTestEnv(TestEnvConfig{Limits: limits, Sink: sink})
	defer env.Close()

	wire := txfixture.MustSignedTransferWire(0)
	result, reason := env.Bank.Forge(wire)
	require.Equal(t, ForgeAccepted, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	require.Empty(t, sink.batches)
	require.Equal(t, 1, env.Bank.EntryBuilder().PendingCount())

	require.Equal(t, costmodel.ExceedNone, env.Bank.PrepareSchedule(200))
	require.Len(t, sink.batches, 1)
	require.Zero(t, env.Bank.EntryBuilder().PendingCount())
}

func TestPrepareScheduleReservesAndRebatesWhenNotIncluded(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()

	wire := txfixture.MustSignedTransferWire(0)
	result, reason := env.Bank.Forge(wire)
	require.Equal(t, ForgeAccepted, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	included := env.Bank.EntryBytes()
	require.Greater(t, included, 0)
	require.Zero(t, env.Bank.EntryBuilder().ReservedBytes())

	require.Equal(t, costmodel.ExceedNone, env.Bank.PrepareSchedule(len(wire)))
	require.Greater(t, env.Bank.EntryBytes(), included)
	require.Greater(t, env.Bank.EntryBuilder().ReservedBytes(), 0)

	result, reason = env.Bank.Forge(wire)
	require.Equal(t, ForgeDroppedAlreadyProcessed, result)
	require.Equal(t, costmodel.ExceedNone, reason)
	require.Equal(t, included, env.Bank.EntryBytes())
	require.Zero(t, env.Bank.EntryBuilder().ReservedBytes())
	require.Len(t, env.Bank.ForgedTransactions(), 1)
}

func TestPrepareScheduleRejectsSlotEntryBudget(t *testing.T) {
	limits := costmodel.DefaultLimits()
	limits.MaxEntryBytes = 80
	env := NewTestEnv(TestEnvConfig{Limits: limits})
	defer env.Close()

	wire := txfixture.MustSignedTransferWire(0)
	require.Equal(t, costmodel.ExceedBatchBytes, env.Bank.PrepareSchedule(len(wire)))
	require.Zero(t, env.Bank.EntryBytes())
	result, reason := env.Bank.Forge(wire)
	assert.Equal(t, ForgeDroppedCost, result)
	assert.Equal(t, costmodel.ExceedBatchBytes, reason)
	assert.Empty(t, env.Bank.ForgedTransactions())
}

func TestWorkingBankFlushesOnBatchLimit(t *testing.T) {
	sink := &captureSink{}
	limits := costmodel.DefaultLimits()
	limits.MaxBatchBytes = 300
	env := NewTestEnv(TestEnvConfig{Limits: limits, Sink: sink})
	defer env.Close()

	for seq := uint64(0); seq < 3; seq++ {
		wire := txfixture.MustSignedTransferWire(seq)
		result, _ := env.Bank.Forge(wire)
		require.Equal(t, ForgeAccepted, result)
	}

	assert.GreaterOrEqual(t, len(sink.batches), 1)
	totalTxns := 0
	for _, batch := range sink.batches {
		for _, entry := range batch {
			totalTxns += len(entry.Txns)
		}
	}
	assert.GreaterOrEqual(t, totalTxns, 1)
}

func TestEntryBuilderFlush(t *testing.T) {
	builder := NewEntryBuilder(costmodel.DefaultLimits(), solana.Hash{0xcd})
	wire := txfixture.MustSignedTransferWire(0)
	tx, err := solana.TransactionFromBytes(wire)
	require.NoError(t, err)
	_, _, _ = builder.Append(*tx, len(wire))

	entries, batchBytes := builder.Flush()
	require.Len(t, entries, 1)
	assert.Equal(t, 1, len(entries[0].Txns))
	assert.Greater(t, batchBytes, 0)
}

func TestControllerWorkingBank(t *testing.T) {
	controller := NewController()
	assert.Nil(t, controller.WorkingBank())

	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()
	controller.SetWorkingBank(env.Bank)
	assert.Equal(t, env.Bank, controller.WorkingBank())
}
