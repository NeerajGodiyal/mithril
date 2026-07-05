package turbine

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/gagliardetto/solana-go"
)

// buildSignedTestShred fabricates a merkle data shred whose root is signed by
// a deterministic test leader (mirrors TestMerkleShredSignatureVerification).
func buildSignedTestShred(t *testing.T, slot uint64, payloadByte byte) (*Shred, solana.PublicKey) {
	t.Helper()
	packet := make([]byte, dataPayloadSize)
	packet[shredVariantOffset] = merkleDataVariant
	packet[dataFlagsOffset] = shredFlagLastShredInSlot
	binary.LittleEndian.PutUint64(packet[shredSlotOffset:shredSlotOffset+8], slot)
	binary.LittleEndian.PutUint32(packet[shredIndexOffset:shredIndexOffset+4], 0)
	binary.LittleEndian.PutUint16(packet[shredVersionOffset:shredVersionOffset+2], 1)
	binary.LittleEndian.PutUint32(packet[shredFECSetIndexOffset:shredFECSetIndexOffset+4], 0)
	binary.LittleEndian.PutUint16(packet[dataParentOffsetOffset:dataParentOffsetOffset+2], 1)
	binary.LittleEndian.PutUint16(packet[dataSizeOffset:dataSizeOffset+2], dataHeaderSize+4)
	packet[dataHeaderSize] = payloadByte

	shred, err := ParseShred(packet)
	if err != nil {
		t.Fatalf("ParseShred returned error: %v", err)
	}
	root, err := shred.MerkleRoot()
	if err != nil {
		t.Fatalf("MerkleRoot returned error: %v", err)
	}

	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	copy(packet[shredSignatureOffset:shredSignatureSize], ed25519.Sign(privateKey, root[:]))
	var leader solana.PublicKey
	copy(leader[:], privateKey.Public().(ed25519.PublicKey))

	signed, err := ParseShred(packet)
	if err != nil {
		t.Fatalf("ParseShred signed packet returned error: %v", err)
	}
	return signed, leader
}

func TestShredSigCacheVerifiesOncePerRoot(t *testing.T) {
	shred, leader := buildSignedTestShred(t, 10, 0x42)
	cache := &shredSigCache{}

	if err := cache.verifyShred(shred, leader); err != nil {
		t.Fatalf("first verify returned error: %v", err)
	}
	if hits, verifies := cache.stats(); hits != 0 || verifies != 1 {
		t.Fatalf("after first verify: hits=%d verifies=%d, want 0/1", hits, verifies)
	}

	// Same (leader, root, signature) triple — stands in for the ~63 sibling
	// shreds of the FEC set — must authenticate off the cache.
	if err := cache.verifyShred(shred, leader); err != nil {
		t.Fatalf("cached verify returned error: %v", err)
	}
	if hits, verifies := cache.stats(); hits != 1 || verifies != 1 {
		t.Fatalf("after cached verify: hits=%d verifies=%d, want 1/1", hits, verifies)
	}
}

// Tampered content yields a different recomputed root, so it can never ride
// a cached entry — the proof walk per shred is what keeps the cache sound.
func TestShredSigCacheRejectsTamperedContent(t *testing.T) {
	shred, leader := buildSignedTestShred(t, 10, 0x42)
	cache := &shredSigCache{}
	if err := cache.verifyShred(shred, leader); err != nil {
		t.Fatalf("priming verify returned error: %v", err)
	}

	tampered, _ := buildSignedTestShred(t, 10, 0x43) // different payload byte
	tampered.Signature = shred.Signature             // carries the GOOD signature
	if err := cache.verifyShred(tampered, leader); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered verify error = %v, want ErrInvalidSignature", err)
	}
	if hits, _ := cache.stats(); hits != 0 {
		t.Fatalf("tampered shred hit the cache (hits=%d)", hits)
	}
}

// Verification failures are never cached: a wrong leader fails the full
// ed25519 check every time.
func TestShredSigCacheDoesNotCacheFailures(t *testing.T) {
	shred, _ := buildSignedTestShred(t, 10, 0x42)
	var wrongLeader solana.PublicKey
	wrongLeader[0] = 0xAA
	cache := &shredSigCache{}

	for round := 0; round < 2; round++ {
		if err := cache.verifyShred(shred, wrongLeader); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("round %d: error = %v, want ErrInvalidSignature", round, err)
		}
	}
	if hits, verifies := cache.stats(); hits != 0 || verifies != 2 {
		t.Fatalf("hits=%d verifies=%d, want 0/2 (failures re-verify)", hits, verifies)
	}
}

func TestShredSigCacheRotationPromotesHotEntries(t *testing.T) {
	cache := &shredSigCache{}
	hot := shredSigCacheKey{}
	hot.root[0] = 0xEE

	cache.mu.Lock()
	cache.addLocked(hot)
	// Fill the current generation so the next add rotates it into prev.
	for i := 0; len(cache.cur) < shredSigCacheGenCap; i++ {
		var key shredSigCacheKey
		binary.LittleEndian.PutUint64(key.root[:], uint64(i))
		key.root[31] = 0x01
		cache.addLocked(key)
	}
	var overflow shredSigCacheKey
	overflow.root[0] = 0xFF
	cache.addLocked(overflow) // triggers rotation: cur -> prev
	if _, inCur := cache.cur[hot]; inCur {
		t.Fatal("hot entry should have rotated into prev")
	}
	if _, inPrev := cache.prev[hot]; !inPrev {
		t.Fatal("hot entry missing from prev after rotation")
	}
	cache.mu.Unlock()

	// A prev hit promotes back into cur so a set straddling the rotation
	// keeps costing zero ed25519.
	if _, ok := cache.prev[hot]; !ok {
		t.Fatal("precondition: hot in prev")
	}
	cache.mu.Lock()
	cache.addLocked(hot)
	_, promoted := cache.cur[hot]
	cache.mu.Unlock()
	if !promoted {
		t.Fatal("promotion did not restore the entry to cur")
	}
}
