package sealevel

import (
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/cu"
	"github.com/Overclock-Validator/mithril/pkg/features"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestVerifyVoteBLSProofOfPossessionFiredancerVector(t *testing.T) {
	msgHex := "414c50454e474c4f570123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdefb8778284f744f6ae2791145183ef8fcb66dcd6602da8ca1add3e6828904db482708fb1d9bd2cbeb72320cdef56d173bc"
	proofHex := "b21b2bc4933e1d2cd32e9b976cc89a98d14f45c89356bb67afab0bc48a6ff9c2d3c4d2394d68706077e5dd7596459da70227c70f2f14adbfbcf6b46ae34f970f88b49dd8185f705333f682eb27674e8abbdf21519dd01424f6993713c9e4632d"
	pubkeyHex := "b8778284f744f6ae2791145183ef8fcb66dcd6602da8ca1add3e6828904db482708fb1d9bd2cbeb72320cdef56d173bc"

	msg, err := hex.DecodeString(msgHex)
	require.NoError(t, err)
	proofBytes, err := hex.DecodeString(proofHex)
	require.NoError(t, err)
	pubkeyBytes, err := hex.DecodeString(pubkeyHex)
	require.NoError(t, err)

	var votePubkey solana.PublicKey
	copy(votePubkey[:], msg[9:9+32])

	var args VoterWithBLSArgs
	copy(args.BlsPubkeyCompressed[:], pubkeyBytes)
	copy(args.BlsProofOfPossession[:], proofBytes)

	require.NoError(t, verifyVoteBLSProofOfPossession(votePubkey, &args))
}

func TestVoteAuthorizeCheckedUnmarshalVoterWithBLS(t *testing.T) {
	var args VoterWithBLSArgs
	for i := range args.BlsPubkeyCompressed {
		args.BlsPubkeyCompressed[i] = byte(i + 1)
	}
	for i := range args.BlsProofOfPossession {
		args.BlsProofOfPossession[i] = byte(i + 2)
	}

	raw := binary.LittleEndian.AppendUint32(nil, VoteProgramInstrTypeAuthorizeChecked)
	raw = binary.LittleEndian.AppendUint32(raw, VoteAuthorizeTypeVoterWithBLS)
	raw = append(raw, args.BlsPubkeyCompressed[:]...)
	raw = append(raw, args.BlsProofOfPossession[:]...)

	decoder := bin.NewBinDecoder(raw[4:])
	var authorize VoteAuthorizeKind
	require.NoError(t, authorize.UnmarshalWithDecoder(decoder))
	require.Equal(t, uint32(VoteAuthorizeTypeVoterWithBLS), authorize.Type)
	require.NotNil(t, authorize.VoterWithBLS)
	require.Equal(t, args.BlsPubkeyCompressed, authorize.VoterWithBLS.BlsPubkeyCompressed)
	require.Equal(t, args.BlsProofOfPossession, authorize.VoterWithBLS.BlsProofOfPossession)
}

func voteBLSTestPubkey(seed byte) solana.PublicKey {
	var raw [32]byte
	raw[0] = seed
	return solana.PublicKeyFromBytes(raw[:])
}

func voteBLSTestV4AccountData(t *testing.T, votePubkey, voter, withdrawer solana.PublicKey, epoch uint64) []byte {
	t.Helper()

	var authVoters AuthorizedVoters
	authVoters.AuthorizedVoters.Set(epoch, voter)

	versioned := &VoteStateVersions{
		Type: VoteStateVersionV4,
		V4: VoteState4{
			NodePubkey:                    votePubkey,
			AuthorizedWithdrawer:          withdrawer,
			AuthorizedVoters:              authVoters,
			InflationRewardsCollector:     votePubkey,
			BlockRevenueCollector:           votePubkey,
			InflationRewardsCommissionBps: 0,
			BlockRevenueCommissionBps:     10000,
		},
	}

	data, err := marshalVersionedVoteState(versioned)
	require.NoError(t, err)

	padded := make([]byte, VoteStateV3Size)
	copy(padded, data)
	return padded
}

func firedancerVoteAuthorizeCheckedArgs(t *testing.T) (solana.PublicKey, VoterWithBLSArgs) {
	t.Helper()

	msgHex := "414c50454e474c4f570123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdefb8778284f744f6ae2791145183ef8fcb66dcd6602da8ca1add3e6828904db482708fb1d9bd2cbeb72320cdef56d173bc"
	proofHex := "b21b2bc4933e1d2cd32e9b976cc89a98d14f45c89356bb67afab0bc48a6ff9c2d3c4d2394d68706077e5dd7596459da70227c70f2f14adbfbcf6b46ae34f970f88b49dd8185f705333f682eb27674e8abbdf21519dd01424f6993713c9e4632d"
	pubkeyHex := "b8778284f744f6ae2791145183ef8fcb66dcd6602da8ca1add3e6828904db482708fb1d9bd2cbeb72320cdef56d173bc"

	msg, err := hex.DecodeString(msgHex)
	require.NoError(t, err)
	proofBytes, err := hex.DecodeString(proofHex)
	require.NoError(t, err)
	pubkeyBytes, err := hex.DecodeString(pubkeyHex)
	require.NoError(t, err)

	var votePubkey solana.PublicKey
	copy(votePubkey[:], msg[9:9+32])

	var args VoterWithBLSArgs
	copy(args.BlsPubkeyCompressed[:], pubkeyBytes)
	copy(args.BlsProofOfPossession[:], proofBytes)
	return votePubkey, args
}

func encodeVoteAuthorizeCheckedInstruction(authorize VoteAuthorizeKind) []byte {
	instrData := binary.LittleEndian.AppendUint32(nil, VoteProgramInstrTypeAuthorizeChecked)
	instrData = binary.LittleEndian.AppendUint32(instrData, authorize.Type)
	if authorize.Type == VoteAuthorizeTypeVoterWithBLS && authorize.VoterWithBLS != nil {
		instrData = append(instrData, authorize.VoterWithBLS.BlsPubkeyCompressed[:]...)
		instrData = append(instrData, authorize.VoterWithBLS.BlsProofOfPossession[:]...)
	}
	return instrData
}

func TestVoteProgramAuthorizeCheckedVoterWithBLS(t *testing.T) {
	votePubkey, blsArgs := firedancerVoteAuthorizeCheckedArgs(t)
	voter := voteBLSTestPubkey(7)

	oldSysvarCache := SysvarCache
	SysvarCache.Clock.Sysvar = &SysvarClock{Slot: 100, Epoch: 10, LeaderScheduleEpoch: 11}
	defer func() { SysvarCache = oldSysvarCache }()

	programAcct := accounts.Account{
		Key:        a.VoteProgramAddr,
		Lamports:   1,
		Owner:      a.NativeLoaderAddr,
		Executable: true,
		RentEpoch:  100,
	}
	voteAcct := accounts.Account{
		Key:       votePubkey,
		Lamports:  1,
		Data:      voteBLSTestV4AccountData(t, votePubkey, voter, voter, 10),
		Owner:     a.VoteProgramAddr,
		RentEpoch: 100,
	}
	clockAcct := accounts.Account{
		Key:       SysvarClockAddr,
		Lamports:  1,
		Data:      make([]byte, 40),
		Owner:     a.SysvarOwnerAddr,
		RentEpoch: 100,
	}
	voterAcct := accounts.Account{
		Key:       voter,
		Lamports:  1,
		Data:      make([]byte, 0),
		Owner:     a.SystemProgramAddr,
		RentEpoch: 100,
	}

	transactionAccts := NewTransactionAccounts([]accounts.Account{programAcct, voteAcct, clockAcct, voterAcct, voterAcct})
	acctMetas := []AccountMeta{
		{Pubkey: voteAcct.Key, IsWritable: true},
		{Pubkey: clockAcct.Key, IsWritable: false},
		{Pubkey: voterAcct.Key, IsSigner: true},
		{Pubkey: voterAcct.Key, IsSigner: true},
	}
	instructionAccts := InstructionAcctsFromAccountMetas(acctMetas, *transactionAccts)
	txCtx := NewTransactionCtx(*transactionAccts, 5, 64)

	ft := features.NewFeaturesDefault()
	ft.EnableFeature(features.VoteStateV4, 0)

	execCtx := ExecutionCtx{
		TransactionContext: txCtx,
		ComputeMeter:       cu.NewComputeMeter(1_000_000),
		Features:           *ft,
		ModifiedVoteStates: make(map[solana.PublicKey]*VoteStateVersions),
	}

	instrData := encodeVoteAuthorizeCheckedInstruction(VoteAuthorizeKind{
		Type:         VoteAuthorizeTypeVoterWithBLS,
		VoterWithBLS: &blsArgs,
	})

	err := execCtx.ProcessInstruction(instrData, instructionAccts, []uint64{0})
	require.NoError(t, err)

	updatedVoteAcct, getErr := txCtx.Accounts.GetAccount(1)
	require.NoError(t, getErr)

	updatedVoteState, unmarshalErr := UnmarshalVersionedVoteState(updatedVoteAcct.Data)
	require.NoError(t, unmarshalErr)
	require.Equal(t, uint32(VoteStateVersionV4), updatedVoteState.Type)
	require.NotNil(t, updatedVoteState.V4.BlsPubkeyCompressed)
	require.Equal(t, blsArgs.BlsPubkeyCompressed, *updatedVoteState.V4.BlsPubkeyCompressed)
}

func TestVoteProgramAuthorizeCheckedVoterWithBLSRejectsLegacyVoterWhenBLSSet(t *testing.T) {
	votePubkey, blsArgs := firedancerVoteAuthorizeCheckedArgs(t)
	voter := voteBLSTestPubkey(7)

	oldSysvarCache := SysvarCache
	SysvarCache.Clock.Sysvar = &SysvarClock{Slot: 100, Epoch: 10, LeaderScheduleEpoch: 11}
	defer func() { SysvarCache = oldSysvarCache }()

	programAcct := accounts.Account{
		Key:        a.VoteProgramAddr,
		Lamports:   1,
		Owner:      a.NativeLoaderAddr,
		Executable: true,
		RentEpoch:  100,
	}

	data := voteBLSTestV4AccountData(t, votePubkey, voter, voter, 10)
	versioned, err := UnmarshalVersionedVoteState(data)
	require.NoError(t, err)
	versioned.V4.BlsPubkeyCompressed = &blsArgs.BlsPubkeyCompressed
	data, err = marshalVersionedVoteState(versioned)
	require.NoError(t, err)
	padded := make([]byte, VoteStateV3Size)
	copy(padded, data)

	voteAcct := accounts.Account{
		Key:       votePubkey,
		Lamports:  1,
		Data:      padded,
		Owner:     a.VoteProgramAddr,
		RentEpoch: 100,
	}
	clockAcct := accounts.Account{
		Key:       SysvarClockAddr,
		Lamports:  1,
		Data:      make([]byte, 40),
		Owner:     a.SysvarOwnerAddr,
		RentEpoch: 100,
	}
	voterAcct := accounts.Account{
		Key:       voter,
		Lamports:  1,
		Data:      make([]byte, 0),
		Owner:     a.SystemProgramAddr,
		RentEpoch: 100,
	}

	transactionAccts := NewTransactionAccounts([]accounts.Account{programAcct, voteAcct, clockAcct, voterAcct, voterAcct})
	acctMetas := []AccountMeta{
		{Pubkey: voteAcct.Key, IsWritable: true},
		{Pubkey: clockAcct.Key, IsWritable: false},
		{Pubkey: voterAcct.Key, IsSigner: true},
		{Pubkey: voterAcct.Key, IsSigner: true},
	}
	instructionAccts := InstructionAcctsFromAccountMetas(acctMetas, *transactionAccts)
	txCtx := NewTransactionCtx(*transactionAccts, 5, 64)

	ft := features.NewFeaturesDefault()
	ft.EnableFeature(features.VoteStateV4, 0)

	execCtx := ExecutionCtx{
		TransactionContext: txCtx,
		ComputeMeter:       cu.NewComputeMeter(1_000_000),
		Features:           *ft,
		ModifiedVoteStates: make(map[solana.PublicKey]*VoteStateVersions),
	}

	instrData := encodeVoteAuthorizeCheckedInstruction(VoteAuthorizeKind{Type: VoteAuthorizeTypeVoter})
	err = execCtx.ProcessInstruction(instrData, instructionAccts, []uint64{0})
	require.ErrorIs(t, err, InstrErrInvalidInstructionData)
}

func TestVoteInstrVoteInitV2UnmarshalAlpenglowCreateVoteAccount(t *testing.T) {
	instrHex := "10000000a7685c3dfa874fef3e96fe8504ef37cf04de2b43837a6f7d144dabdbd11cefc9a7685c3dfa874fef3e96fe8504ef37cf04de2b43837a6f7d144dabdbd11cefc990c8ccd1d093ea9ae518446fc1a703b94b5b6098318d79e898ede09d9dc165b486cf464156d960f474386c39640a087d98a5a29e4c57ec214ef9d0ccfca623d26a100c00d7177462f29d21d44c92a87b55981e62faeb627c469dbdb28002117b0827a1dc7f22b7a8307e95830923fec05952c91980fbdb7abda4e526220b9dfa0dea5c868cb86e1d59bd0474be7c81ea56539ccb33aa2bf17ab6c2dd3c1b8ff012b4f8b0f9c571f7e4132d166687d6b810271027"
	raw, err := hex.DecodeString(instrHex)
	require.NoError(t, err)
	require.Len(t, raw, 248)

	identity := solana.MustPublicKeyFromBase58("CGVPB8mbi1yBGmMEP3Sx2muuC9oR3o6zujLP3rt4kV92")
	withdrawer := solana.MustPublicKeyFromBase58("6oz1YJUKTzf9GTrMrahpacNqZhoSZSvgrPVM4oQ6RxCF")

	decoder := bin.NewBinDecoder(raw[4:])
	var voteInit VoteInstrVoteInitV2
	require.NoError(t, voteInit.UnmarshalWithDecoder(decoder))

	require.Equal(t, identity, voteInit.NodePubkey)
	require.Equal(t, identity, voteInit.AuthorizedVoter)
	require.Equal(t, withdrawer, voteInit.AuthorizedWithdrawer)
	require.Equal(t, uint16(10000), voteInit.InflationRewardsCommissionBps)
	require.Equal(t, uint16(10000), voteInit.BlockRevenueCommissionBps)
}

func TestVoteProgramInitializeAccountV2AlpenglowCreateVoteAccount(t *testing.T) {
	instrHex := "10000000a7685c3dfa874fef3e96fe8504ef37cf04de2b43837a6f7d144dabdbd11cefc9a7685c3dfa874fef3e96fe8504ef37cf04de2b43837a6f7d144dabdbd11cefc990c8ccd1d093ea9ae518446fc1a703b94b5b6098318d79e898ede09d9dc165b486cf464156d960f474386c39640a087d98a5a29e4c57ec214ef9d0ccfca623d26a100c00d7177462f29d21d44c92a87b55981e62faeb627c469dbdb28002117b0827a1dc7f22b7a8307e95830923fec05952c91980fbdb7abda4e526220b9dfa0dea5c868cb86e1d59bd0474be7c81ea56539ccb33aa2bf17ab6c2dd3c1b8ff012b4f8b0f9c571f7e4132d166687d6b810271027"
	instrData, err := hex.DecodeString(instrHex)
	require.NoError(t, err)

	votePubkey := solana.MustPublicKeyFromBase58("Fd8wBPGcwXprLWYPT7hZD2c5494ptfnNzhj5WWLa8t6N")
	identity := solana.MustPublicKeyFromBase58("CGVPB8mbi1yBGmMEP3Sx2muuC9oR3o6zujLP3rt4kV92")
	withdrawer := solana.MustPublicKeyFromBase58("6oz1YJUKTzf9GTrMrahpacNqZhoSZSvgrPVM4oQ6RxCF")

	oldSysvarCache := SysvarCache
	SysvarCache.Clock.Sysvar = &SysvarClock{Slot: 100, Epoch: 10, LeaderScheduleEpoch: 11}
	defaultRent := NewDefaultRentSysvar()
	SysvarCache.Rent.Sysvar = &defaultRent
	defer func() { SysvarCache = oldSysvarCache }()

	programAcct := accounts.Account{
		Key:        a.VoteProgramAddr,
		Lamports:   1,
		Owner:      a.NativeLoaderAddr,
		Executable: true,
		RentEpoch:  100,
	}
	identityAcct := accounts.Account{
		Key:       identity,
		Lamports:  defaultRent.MinimumBalance(0),
		Data:      make([]byte, 0),
		Owner:     a.SystemProgramAddr,
		RentEpoch: 100,
	}
	voteAcct := accounts.Account{
		Key:       votePubkey,
		Lamports:  defaultRent.MinimumBalance(VoteStateV3Size),
		Data:      make([]byte, VoteStateV3Size),
		Owner:     a.VoteProgramAddr,
		RentEpoch: 100,
	}

	transactionAccts := NewTransactionAccounts([]accounts.Account{programAcct, identityAcct, voteAcct})
	acctMetas := []AccountMeta{
		{Pubkey: voteAcct.Key, IsWritable: true},
		{Pubkey: identityAcct.Key, IsSigner: true},
		{Pubkey: voteAcct.Key, IsWritable: true},
		{Pubkey: identityAcct.Key, IsWritable: true},
	}
	instructionAccts := InstructionAcctsFromAccountMetas(acctMetas, *transactionAccts)
	txCtx := NewTransactionCtx(*transactionAccts, 5, 64)

	ft := features.NewFeaturesDefault()
	ft.EnableFeature(features.VoteStateV4, 0)

	execCtx := ExecutionCtx{
		TransactionContext: txCtx,
		ComputeMeter:       cu.NewComputeMeter(1_000_000),
		Features:           *ft,
		ModifiedVoteStates: make(map[solana.PublicKey]*VoteStateVersions),
	}

	require.NoError(t, execCtx.ProcessInstruction(instrData, instructionAccts, []uint64{0}))

	updatedVoteAcct, getErr := txCtx.Accounts.GetAccount(2)
	require.NoError(t, getErr)

	updatedVoteState, unmarshalErr := UnmarshalVersionedVoteState(updatedVoteAcct.Data)
	require.NoError(t, unmarshalErr)
	require.Equal(t, uint32(VoteStateVersionV4), updatedVoteState.Type)
	require.Equal(t, identity, updatedVoteState.V4.NodePubkey)
	require.Equal(t, withdrawer, updatedVoteState.V4.AuthorizedWithdrawer)
	require.Equal(t, votePubkey, updatedVoteState.V4.InflationRewardsCollector)
	require.Equal(t, identity, updatedVoteState.V4.BlockRevenueCollector)
	require.Equal(t, uint16(10000), updatedVoteState.V4.InflationRewardsCommissionBps)
	require.Equal(t, uint16(10000), updatedVoteState.V4.BlockRevenueCommissionBps)
	require.NotNil(t, updatedVoteState.V4.BlsPubkeyCompressed)
	require.Equal(
		t,
		instrData[4+32+32:4+32+32+48],
		updatedVoteState.V4.BlsPubkeyCompressed[:],
	)
}
