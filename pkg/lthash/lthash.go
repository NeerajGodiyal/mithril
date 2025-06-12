package lthash

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"unsafe"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/zeebo/blake3"
)

const numElements = 1024

type LtHash struct {
	value [numElements]uint16
}

func (ltHash *LtHash) calculateAcctHash(acct *accounts.Account) []byte {
	hasher := blake3.New()

	var lamportBytes [8]byte
	binary.LittleEndian.PutUint64(lamportBytes[:], acct.Lamports)
	_, _ = hasher.Write(lamportBytes[:])

	var rentEpochBytes [8]byte
	binary.LittleEndian.PutUint64(rentEpochBytes[:], acct.RentEpoch)
	_, _ = hasher.Write(rentEpochBytes[:])

	_, _ = hasher.Write(acct.Data)

	if acct.Executable {
		_, _ = hasher.Write([]byte{1})
	} else {
		_, _ = hasher.Write([]byte{0})
	}

	_, _ = hasher.Write(acct.Owner[:])
	_, _ = hasher.Write(acct.Key[:])

	h := sha256.New()
	h.Write(acct.Data)

	return hasher.Sum(nil)
}

func (ltHash *LtHash) InitWithAcct(acct *accounts.Account) *LtHash {
	h := ltHash.calculateAcctHash(acct)

	hasher := blake3.New()
	hasher.Write(h)

	var data [2048]byte
	digest := hasher.Digest()
	digest.Read(data[:])

	for i := range numElements {
		val := binary.LittleEndian.Uint16(data[i*2 : (i*2)+2])
		ltHash.value[i] = val
	}

	return ltHash
}

func (ltHash *LtHash) InitWithBytes(data []byte) *LtHash {
	hasher := blake3.New()
	hasher.Write(data)

	var output [2048]byte
	digest := hasher.Digest()
	digest.Read(output[:])

	for i := range numElements {
		val := binary.LittleEndian.Uint16(output[i*2 : (i*2)+2])
		ltHash.value[i] = val
	}

	return ltHash
}

func (ltHash *LtHash) initRandom() *LtHash {
	randBytes := unsafe.Slice((*uint8)(unsafe.Pointer(&ltHash.value[0])), numElements*2)
	rand.Read(randBytes)
	return ltHash
}

func (ltHash *LtHash) Clone() *LtHash {
	new := &LtHash{}
	copy(new.value[:], ltHash.value[:])
	return new
}

func (ltHash *LtHash) MixIn(other *LtHash) {
	for i := range numElements {
		ltHash.value[i] = ltHash.value[i] + other.value[i]
	}
}

func (ltHash *LtHash) MixOut(other *LtHash) {
	for i := range numElements {
		ltHash.value[i] = ltHash.value[i] - other.value[i]
	}
}

func (ltHash *LtHash) Add(other *LtHash) *LtHash {
	ltHash.MixIn(other)
	return ltHash
}

func (ltHash *LtHash) Sub(other *LtHash) *LtHash {
	ltHash.MixOut(other)
	return ltHash
}

func (ltHash *LtHash) Checksum() []byte {
	data := unsafe.Slice((*uint8)(unsafe.Pointer(&ltHash.value[0])), numElements*2)
	hasher := blake3.New()
	hasher.Write(data)
	return hasher.Sum(nil)
}

func (ltHash *LtHash) Hash() []byte {
	data := unsafe.Slice((*uint8)(unsafe.Pointer(&ltHash.value[0])), numElements*2)
	return data
}
