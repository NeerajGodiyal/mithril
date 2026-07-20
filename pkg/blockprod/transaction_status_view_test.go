package blockprod

import "github.com/Overclock-Validator/mithril/pkg/replay"

func completeTestTransactionStatuses() *replay.TransactionStatusView {
	return replay.NewTransactionStatusCache().View()
}
