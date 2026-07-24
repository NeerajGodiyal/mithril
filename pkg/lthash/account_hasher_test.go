package lthash

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/gagliardetto/solana-go"
	"github.com/zeebo/blake3"
)

func referenceAccountHash(acct *accounts.Account) []byte {
	output := make([]byte, numElements*2)
	if acct.Lamports == 0 {
		return output
	}

	hasher := blake3.New()
	var lamportBytes [8]byte
	binary.LittleEndian.PutUint64(lamportBytes[:], acct.Lamports)
	_, _ = hasher.Write(lamportBytes[:])
	_, _ = hasher.Write(acct.Data)
	if acct.Executable {
		_, _ = hasher.Write([]byte{1})
	} else {
		_, _ = hasher.Write([]byte{0})
	}
	_, _ = hasher.Write(acct.Owner[:])
	_, _ = hasher.Write(acct.Key[:])
	_, _ = hasher.Digest().Read(output)
	return output
}

func accountHashTestAccount(seed byte, dataLen int) *accounts.Account {
	var key solana.PublicKey
	var owner [32]byte
	for i := range key {
		key[i] = seed + byte(i*3)
		owner[i] = seed ^ byte(i*7)
	}
	data := make([]byte, dataLen)
	for i := range data {
		data[i] = seed + byte(i*11)
	}
	return &accounts.Account{
		Key:        key,
		Lamports:   uint64(seed)*1_000_003 + 17,
		Data:       data,
		Owner:      owner,
		Executable: seed%2 != 0,
		RentEpoch:  uint64(seed) * 19,
	}
}

func TestAccountHasherMatchesReferenceAcrossReuse(t *testing.T) {
	accountsToHash := []*accounts.Account{
		accountHashTestAccount(1, 0),
		accountHashTestAccount(2, 1),
		accountHashTestAccount(3, 63),
		accountHashTestAccount(4, 1024),
		accountHashTestAccount(5, 4097),
	}

	var hasher AccountHasher
	var got LtHash
	for i, acct := range accountsToHash {
		hasher.HashInto(&got, acct)
		if want := referenceAccountHash(acct); !bytes.Equal(got.Hash(), want) {
			t.Fatalf("account %d differs from independent BLAKE3-XOF reference", i)
		}
	}

	// Reusing both objects must reset all hasher and destination state.
	hasher.HashInto(&got, accountsToHash[0])
	if want := referenceAccountHash(accountsToHash[0]); !bytes.Equal(got.Hash(), want) {
		t.Fatal("reused account hasher retained state from a previous account")
	}
}

func TestAccountHasherZeroLamportsClearsReusedDestination(t *testing.T) {
	var hasher AccountHasher
	var got LtHash
	hasher.HashInto(&got, accountHashTestAccount(9, 128))

	zero := accountHashTestAccount(10, 128)
	zero.Lamports = 0
	hasher.HashInto(&got, zero)
	if !bytes.Equal(got.Hash(), make([]byte, numElements*2)) {
		t.Fatal("zero-lamport account did not clear the reused destination")
	}

	// InitWithAcct has the same in-place contract and must return the receiver.
	got.InitWithHash(bytes.Repeat([]byte{0xff}, numElements*2))
	if returned := got.InitWithAcct(zero); returned != &got {
		t.Fatal("InitWithAcct returned a different LtHash for a zero-lamport account")
	}
	if !bytes.Equal(got.Hash(), make([]byte, numElements*2)) {
		t.Fatal("InitWithAcct did not clear its receiver for a zero-lamport account")
	}
}

func TestAccountHasherRentEpochIsNotHashed(t *testing.T) {
	first := accountHashTestAccount(13, 96)
	second := first.Clone()
	second.RentEpoch++

	var hasher AccountHasher
	var firstHash, secondHash LtHash
	hasher.HashInto(&firstHash, first)
	hasher.HashInto(&secondHash, second)
	if !firstHash.Equals(&secondHash) {
		t.Fatal("rent epoch unexpectedly changed the accounts LtHash contribution")
	}
}

var benchmarkAccountHashByte byte
var benchmarkReferenceAccountHash []byte

func BenchmarkAccountHasherHashInto(b *testing.B) {
	acct := accountHashTestAccount(23, 256)
	var hasher AccountHasher
	var output LtHash
	b.ReportAllocs()
	b.SetBytes(int64(8 + len(acct.Data) + 1 + len(acct.Owner) + len(acct.Key)))
	b.ResetTimer()
	for range b.N {
		hasher.HashInto(&output, acct)
	}
	benchmarkAccountHashByte = output.Hash()[0]
}

func BenchmarkAccountHasherLegacyReference(b *testing.B) {
	acct := accountHashTestAccount(23, 256)
	b.ReportAllocs()
	b.SetBytes(int64(8 + len(acct.Data) + 1 + len(acct.Owner) + len(acct.Key)))
	b.ResetTimer()
	for range b.N {
		benchmarkReferenceAccountHash = referenceAccountHash(acct)
	}
}
