package rewardcerts

import "fmt"

type partialCert struct {
	signatures [][]byte
	bits       []bool
	count      int
}

func newPartialCert(maxValidators int) partialCert {
	return partialCert{bits: make([]bool, maxValidators)}
}

func (p *partialCert) wantsVote(rank uint16) bool {
	i := int(rank)
	return i >= 0 && i < len(p.bits) && !p.bits[i]
}

func (p *partialCert) addVote(rank uint16, signature []byte) error {
	i := int(rank)
	if i < 0 || i >= len(p.bits) {
		return fmt.Errorf("rewardcerts: invalid vote rank %d", rank)
	}
	if p.bits[i] {
		return fmt.Errorf("rewardcerts: duplicate vote rank %d", rank)
	}
	if len(signature) == 0 {
		return fmt.Errorf("rewardcerts: empty vote signature")
	}
	p.bits[i] = true
	p.signatures = append(p.signatures, append([]byte(nil), signature...))
	p.count++
	return nil
}

func (p *partialCert) build() (SkipRewardCertificate, error) {
	if p.count == 0 {
		return SkipRewardCertificate{}, errEmptyBitmap
	}
	bitmap, err := encodeSignerStoreBase2(p.bits)
	if err != nil {
		return SkipRewardCertificate{}, err
	}
	signature, err := aggregateVoteSignatures(p.signatures)
	if err != nil {
		return SkipRewardCertificate{}, err
	}
	return SkipRewardCertificate{Signature: signature, Bitmap: bitmap}, nil
}

func (p *partialCert) votesSeen() int {
	return p.count
}
