package blockstream

import (
	"context"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/gagliardetto/solana-go"
)

type Prefetcher struct {
	src        *BlockSource
	accountsDb *accountsdb.AccountsDb
	out        chan *block.Block
}

func NewPrefetcher(
	ctx context.Context,
	b *BlockSource,
	a *accountsdb.AccountsDb,
) *Prefetcher {
	p := &Prefetcher{b, a, make(chan *block.Block)}
	go p.prefetchWorker(ctx)
	return p
}

func (p *Prefetcher) NextBlock() *block.Block {
	return <-p.out
}

func (p *Prefetcher) prefetchWorker(ctx context.Context) {
	for {
		b := p.src.NextBlock()
		select {
		case <-ctx.Done():
			mlog.Log.Infof("prefetch worker exiting: %v", ctx.Err())
			return
		case p.out <- b: // Caller was waiting on NextBlock so return it immediately.
			continue
		default:
		}

		alts := getALTs(b)

		p.accountsDb.Prefetch(alts, b.UniqueAccounts())

		select {
		case <-ctx.Done():
			mlog.Log.Infof("prefetch worker exiting: %v", ctx.Err())
			return
		case p.out <- b:
		}
	}
}

func getALTs(b *block.Block) map[solana.PublicKey]*[256]bool {
	out := make(map[solana.PublicKey]*[256]bool)
	for _, tx := range b.Transactions {
		if !tx.Message.IsVersioned() {
			continue
		}
		for _, alt := range tx.Message.GetAddressTableLookups() {
			if out[alt.AccountKey] == nil {
				out[alt.AccountKey] = new([256]bool)
			}
			for _, wi := range alt.WritableIndexes {
				out[alt.AccountKey][wi] = true
			}
			for _, ri := range alt.ReadonlyIndexes {
				out[alt.AccountKey][ri] = true
			}
		}
	}
	return out
}
