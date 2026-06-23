package consensus

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/gagliardetto/solana-go"
)

func TestRefreshAlpenglowValidatorSetFromGlobalEpochStakes(t *testing.T) {
	const epoch = uint64(7)
	vote := solana.NewWallet().PublicKey()
	set := testAlpenglowValidatorSet()
	set.Epoch = epoch
	compressed := set.Validators[0].BlsPubkeyCompressed

	global.PutEpochStakesEntry(epoch, vote, 100, &epochstakes.VoteAccount{
		NodePubkey:          solana.NewWallet().PublicKey(),
		BlsPubkeyCompressed: &compressed,
	})
	global.PutEpochTotalStake(epoch, 100)

	engine, err := NewEngine(ModeAlpenglowObserver)
	if err != nil {
		t.Fatalf("NewEngine returned error: %v", err)
	}
	if err := RefreshAlpenglowValidatorSet(engine, epoch); err != nil {
		t.Fatalf("RefreshAlpenglowValidatorSet returned error: %v", err)
	}

	observer := engine.(*AlpenglowObserverEngine)
	cert := alpenglow.Certificate{
		Type:   alpenglow.CertificateSkip,
		Slot:   42,
		Bitmap: testAlpenglowSignerBitmap(),
	}
	cert.Signature = testAlpenglowCertificateSignature(t, cert)
	observer.SetAlpenglowEpochLookup(func(uint64) uint64 { return epoch })
	observer.observeVotorMessage(alpenglow.NewCertificateMessage(cert))

	snapshot := observer.Snapshot()
	if snapshot.AlpenglowChain == nil || snapshot.AlpenglowChain.CertificatesAccepted != 1 {
		t.Fatalf("expected accepted certificate after validator set refresh, got %+v", snapshot.AlpenglowChain)
	}
}
