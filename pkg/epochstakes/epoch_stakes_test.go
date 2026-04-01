package epochstakes

import (
	"bytes"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

func testPubkey(seed byte) solana.PublicKey {
	var pk solana.PublicKey
	pk[0] = seed
	return pk
}

func marshalVoteStateVersions(t *testing.T, voteState *sealevel.VoteStateVersions) []byte {
	t.Helper()

	var buf bytes.Buffer
	encoder := bin.NewBinEncoder(&buf)
	if err := voteState.MarshalWithEncoder(encoder); err != nil {
		t.Fatalf("marshal vote state: %v", err)
	}
	return buf.Bytes()
}

func TestEpochVoteAccountRoundTripPreservesFullData(t *testing.T) {
	voteState := &sealevel.VoteStateVersions{
		Type: sealevel.VoteStateVersionCurrent,
		Current: sealevel.VoteState{
			NodePubkey:    testPubkey(1),
			LastTimestamp: sealevel.BlockTimestamp{Slot: 1234, Timestamp: 5678},
		},
	}

	var owner [32]byte
	copy(owner[:], a.VoteProgramAddr[:])

	votePubkey := testPubkey(9)
	acct := &accounts.Account{
		Slot:       99,
		Key:        votePubkey,
		Lamports:   42,
		Data:       marshalVoteStateVersions(t, voteState),
		Owner:      owner,
		Executable: false,
		RentEpoch:  7,
	}

	voteAcct, err := NewVoteAccountFromAccount(acct)
	if err != nil {
		t.Fatalf("NewVoteAccountFromAccount: %v", err)
	}

	cache := NewEpochStakesCache()
	cache.PutStake(5, votePubkey, 1000)
	cache.PutVoteAccount(5, votePubkey, voteAcct)
	cache.PutTotalEpochStake(5, 1000)

	serialized, err := cache.SerializeEpoch(5)
	if err != nil {
		t.Fatalf("SerializeEpoch: %v", err)
	}

	loaded := NewEpochStakesCache()
	if _, err := loaded.DeserializeAndLoadEpoch(serialized); err != nil {
		t.Fatalf("DeserializeAndLoadEpoch: %v", err)
	}

	reloaded := loaded.EpochStakesAccts(5)[votePubkey]
	if reloaded == nil {
		t.Fatalf("reloaded vote account missing")
	}
	if !bytes.Equal(reloaded.Data, acct.Data) {
		t.Fatalf("vote account data mismatch after round trip")
	}
	if reloaded.NodePubkey != voteState.Current.NodePubkey {
		t.Fatalf("node pubkey mismatch: got %s want %s", reloaded.NodePubkey, voteState.Current.NodePubkey)
	}

	reloadedVoteState, err := reloaded.VoteState()
	if err != nil {
		t.Fatalf("VoteState: %v", err)
	}
	if reloadedVoteState.Type != sealevel.VoteStateVersionCurrent {
		t.Fatalf("vote state type mismatch: got %d want %d", reloadedVoteState.Type, sealevel.VoteStateVersionCurrent)
	}
	if reloadedVoteState.Current.LastTimestamp != voteState.Current.LastTimestamp {
		t.Fatalf("last timestamp mismatch: got %+v want %+v", reloadedVoteState.Current.LastTimestamp, voteState.Current.LastTimestamp)
	}
}

func TestEpochVoteAccountToAccountClonesData(t *testing.T) {
	voteAcct := &VoteAccount{
		Lamports:   55,
		Data:       []byte{1, 2, 3, 4},
		NodePubkey: testPubkey(2),
		Owner:      testPubkey(3),
		Executable: 1,
		RentEpoch:  11,
	}

	votePubkey := testPubkey(8)
	acct := voteAcct.ToAccount(votePubkey, 777)
	if acct == nil {
		t.Fatalf("ToAccount returned nil")
	}
	if acct.Key != votePubkey || acct.Slot != 777 || acct.Lamports != 55 || !acct.Executable || acct.RentEpoch != 11 {
		t.Fatalf("unexpected account fields: %+v", acct)
	}

	acct.Data[0] = 99
	if voteAcct.Data[0] == 99 {
		t.Fatalf("ToAccount did not clone data")
	}
}
