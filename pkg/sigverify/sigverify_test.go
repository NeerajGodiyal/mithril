package sigverify

import (
	stded25519 "crypto/ed25519"
	"crypto/rand"
	"fmt"
	"testing"

	"filippo.io/edwards25519"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// smallOrderForgery builds a signature that Go's stdlib accepts and that
// mainnet rejects, which is the entire reason this package exists.
//
// The construction: let the public key A be the identity point. The
// verification equation is [s]B == R + [k]A, and [k]A is the identity for every
// k when A is the identity — so the challenge drops out and ANY (R, s) with
// R == [s]B satisfies it, for ANY message, with no private key involved.
//
// stdlib implements exactly that equation and accepts. ed25519-dalek's
// verify_strict — what Agave and therefore mainnet use — additionally rejects a
// small-order A, so it refuses. A validator that accepts these is accepting
// transactions mainnet does not.
func smallOrderForgery(tb testing.TB, message []byte) (pub [32]byte, sig []byte) {
	tb.Helper()

	// Canonical encoding of the identity: y = 1, sign bit clear.
	pub[0] = 1

	uniform := make([]byte, 64)
	_, err := rand.Read(uniform)
	require.NoError(tb, err)
	s, err := edwards25519.NewScalar().SetUniformBytes(uniform)
	require.NoError(tb, err)

	r := (&edwards25519.Point{}).ScalarBaseMult(s)

	sig = make([]byte, 64)
	copy(sig[:32], r.Bytes())
	copy(sig[32:], s.Bytes())
	return pub, sig
}

func TestSmallOrderForgeryIsAcceptedByStdlibAndRejectedHere(t *testing.T) {
	message := []byte("transfer everything")
	pub, sig := smallOrderForgery(t, message)

	require.True(t, stded25519.Verify(pub[:], message, sig),
		"precondition: the forgery must be accepted by stdlib, or the test proves nothing")

	assert.False(t, VerifyOne(&pub, message, sig),
		"strict verification must reject a small-order public key")

	var batch Batch
	batch.Add(&pub, message, sig)
	assert.False(t, batch.Verify(), "batch path must reject it too")
	assert.False(t, batch.OK(0))
}

// The bypass switch is a rollback to the pre-existing behaviour, divergence
// included. Pinning that here keeps it an informed choice rather than a
// surprise, and fails loudly if someone later "fixes" the bypass into
// something that is no longer a faithful rollback.
func TestStdlibBypassReintroducesTheDivergence(t *testing.T) {
	bypass.Store(true)
	t.Cleanup(func() { bypass.Store(false) })

	message := []byte("transfer everything")
	pub, sig := smallOrderForgery(t, message)

	assert.True(t, VerifyOne(&pub, message, sig),
		"backend=stdlib is a rollback: it accepts what stdlib accepts, small-order included")
}

type signedMessage struct {
	pub   [32]byte
	msg   []byte
	sig   []byte
	valid bool
}

func makeSigned(tb testing.TB, index int, valid bool) signedMessage {
	tb.Helper()
	pubKey, privKey, err := stded25519.GenerateKey(rand.Reader)
	require.NoError(tb, err)

	msg := []byte(fmt.Sprintf("message number %d", index))
	sig := stded25519.Sign(privKey, msg)
	if !valid {
		// Flip a bit in s rather than truncating, so the signature stays
		// well-formed and is rejected on the equation rather than on a length
		// or range precheck.
		sig[40] ^= 0x01
	}
	var pub [32]byte
	copy(pub[:], pubKey)
	return signedMessage{pub: pub, msg: msg, sig: sig, valid: valid}
}

func TestValidSignaturesAgreeWithStdlib(t *testing.T) {
	for i := 0; i < 16; i++ {
		item := makeSigned(t, i, true)
		require.True(t, stded25519.Verify(item.pub[:], item.msg, item.sig))
		assert.True(t, VerifyOne(&item.pub, item.msg, item.sig),
			"honest signature %d must verify", i)
	}
}

// Batching must not change any verdict. This is the property that lets the
// replay pool keep its panic-with-exact-signer contract: a batch reports
// per-signature results, and they have to be the results the caller would have
// got one at a time.
func TestBatchVerdictsMatchPerItemVerdicts(t *testing.T) {
	// Widths straddling the x4 and x8 group boundaries, plus the tails either
	// side of them, are where a lane-mapping bug would hide.
	for _, width := range []int{1, 2, 3, 4, 5, 7, 8, 9, 12, 16, 17, 31, 64} {
		t.Run(fmt.Sprintf("width=%d", width), func(t *testing.T) {
			// Put the single invalid signature at every position in turn, so a
			// verdict written to the wrong lane cannot pass by symmetry.
			for badIndex := 0; badIndex < width; badIndex++ {
				items := make([]signedMessage, width)
				for i := range items {
					items[i] = makeSigned(t, i, i != badIndex)
				}

				var batch Batch
				for i := range items {
					batch.Add(&items[i].pub, items[i].msg, items[i].sig)
				}
				all := batch.Verify()

				assert.False(t, all, "batch containing an invalid signature must not report all-valid")
				for i, item := range items {
					assert.Equal(t, item.valid, batch.OK(i),
						"width=%d badIndex=%d: verdict for item %d", width, badIndex, i)
					assert.Equal(t, VerifyOne(&item.pub, item.msg, item.sig), batch.OK(i),
						"width=%d badIndex=%d: batch and single disagree at %d", width, badIndex, i)
				}
			}
		})
	}
}

func TestAllValidBatchReportsAllValid(t *testing.T) {
	for _, width := range []int{1, 4, 8, 9, 64} {
		var batch Batch
		items := make([]signedMessage, width)
		for i := range items {
			items[i] = makeSigned(t, i, true)
			batch.Add(&items[i].pub, items[i].msg, items[i].sig)
		}
		assert.True(t, batch.Verify(), "width=%d: every signature is honest", width)
	}
}

func TestBatchResetRetainsCapacityAndClearsVerdicts(t *testing.T) {
	var batch Batch
	item := makeSigned(t, 0, true)
	for i := 0; i < 8; i++ {
		batch.Add(&item.pub, item.msg, item.sig)
	}
	require.True(t, batch.Verify())
	capacityBefore := cap(batch.pubs)

	batch.Reset()
	assert.Zero(t, batch.Len())
	assert.Equal(t, capacityBefore, cap(batch.pubs), "Reset must retain capacity")
	assert.Nil(t, batch.pubs[:1][0], "Reset must not pin the previous batch's public keys")

	// A reused batch must not inherit stale verdicts.
	bad := makeSigned(t, 1, false)
	batch.Add(&bad.pub, bad.msg, bad.sig)
	assert.False(t, batch.Verify())
	assert.False(t, batch.OK(0))
}

// Batch is worker-local scratch, so filling it must not allocate once its
// backing arrays have grown. This measures only the accumulation this package
// owns; what a backend allocates internally is that backend's contract and is
// covered by its own suite.
func TestBatchAccumulationAllocatesNothingAfterWarmup(t *testing.T) {
	var batch Batch
	items := make([]signedMessage, MaxDrain)
	for i := range items {
		items[i] = makeSigned(t, i, true)
	}
	fill := func() {
		batch.Reset()
		for i := range items {
			batch.Add(&items[i].pub, items[i].msg, items[i].sig)
		}
	}
	fill() // grow the backing arrays

	allocs := testing.AllocsPerRun(5, fill)
	assert.Zero(t, allocs, "a warmed-up worker batch must not allocate while filling")
}

func TestEmptyBatchIsVacuouslyValid(t *testing.T) {
	var batch Batch
	assert.True(t, batch.Verify())
	assert.Zero(t, batch.Len())
}

func TestVerifyOneRejectsNilKey(t *testing.T) {
	item := makeSigned(t, 0, true)
	assert.False(t, VerifyOne(nil, item.msg, item.sig))
}

func TestDrainTakesWhatIsReadyAndNeverBlocks(t *testing.T) {
	ch := make(chan int, 16)
	for i := 2; i <= 5; i++ {
		ch <- i
	}

	got := Drain(nil, 1, ch, MaxDrain)
	assert.Equal(t, []int{1, 2, 3, 4, 5}, got,
		"Drain must take the queued items and return rather than wait for more")
}

func TestDrainStopsAtMax(t *testing.T) {
	ch := make(chan int, 32)
	for i := 0; i < 32; i++ {
		ch <- i
	}
	got := Drain(nil, -1, ch, 8)
	assert.Len(t, got, 8)
	assert.Equal(t, 25, len(ch), "the untaken items must stay queued for other workers")
}

func TestDrainOnEmptyChannelReturnsJustTheFirstItem(t *testing.T) {
	ch := make(chan int)
	got := Drain(nil, 42, ch, MaxDrain)
	assert.Equal(t, []int{42}, got)
}

func TestDrainHandlesClosedChannel(t *testing.T) {
	ch := make(chan int, 2)
	ch <- 2
	close(ch)
	got := Drain(nil, 1, ch, MaxDrain)
	assert.Equal(t, []int{1, 2}, got, "a closed channel must terminate the drain, not spin")
}

func TestDrainReusesTheDestinationSlice(t *testing.T) {
	ch := make(chan int, 8)
	dst := make([]int, 0, MaxDrain)
	for round := 0; round < 3; round++ {
		ch <- round*10 + 1
		dst = Drain(dst, round*10, ch, MaxDrain)
		require.Len(t, dst, 2)
		assert.Equal(t, round*10, dst[0])
		assert.Equal(t, MaxDrain, cap(dst), "Drain must not reallocate a sufficient buffer")
	}
}
