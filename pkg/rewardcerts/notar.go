package rewardcerts

import (
	"fmt"

	"github.com/gagliardetto/solana-go"
)

type notarEntry struct {
	voted    map[uint16]struct{}
	partials map[solana.Hash]*partialCert
}

func newNotarEntry(maxValidators int) notarEntry {
	return notarEntry{
		voted:    make(map[uint16]struct{}, maxValidators),
		partials: make(map[solana.Hash]*partialCert),
	}
}

func (n *notarEntry) wantsVote(rank uint16) bool {
	_, ok := n.voted[rank]
	return !ok
}

func (n *notarEntry) addVote(rank uint16, signature []byte, blockID solana.Hash, maxValidators int) error {
	if _, ok := n.voted[rank]; ok {
		return fmt.Errorf("rewardcerts: duplicate notar vote rank %d", rank)
	}
	partial := n.partials[blockID]
	if partial == nil {
		p := newPartialCert(maxValidators)
		partial = &p
		n.partials[blockID] = partial
	}
	if err := partial.addVote(rank, signature); err != nil {
		return err
	}
	n.voted[rank] = struct{}{}
	return nil
}

func (n *notarEntry) votesSeen() int {
	return len(n.voted)
}

func (n *notarEntry) buildCert(rewardSlot uint64) (NotarRewardCertificate, error) {
	var bestBlock solana.Hash
	var best *partialCert
	for blockID, partial := range n.partials {
		if best == nil || partial.votesSeen() > best.votesSeen() {
			id := blockID
			bestBlock = id
			best = partial
		}
	}
	if best == nil {
		return NotarRewardCertificate{}, errEmptyBitmap
	}
	skipCert, err := best.build()
	if err != nil {
		return NotarRewardCertificate{}, err
	}
	return NotarRewardCertificate{
		Slot:      rewardSlot,
		BlockID:   bestBlock,
		Signature: skipCert.Signature,
		Bitmap:    skipCert.Bitmap,
	}, nil
}
