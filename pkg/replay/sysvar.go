package replay

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

const nsInSeconds = 1000000000

const maxAllowableDriftFast = 25
const maxAllowableDriftSlow = 150

type lastTimestampData struct {
	Timestamp int64
	Stake     uint64
}

func updateClockSysvar(clock *sealevel.SysvarClock, accountsDb *accountsdb.AccountsDb, block *Block) error {
	epochScheduleAcct, err := accountsDb.GetAccount(block.Slot, sealevel.SysvarEpochScheduleAddr)
	if err != nil {
		panic("unable to retrieve epoch schedule sysvar acct when updating clock sysvar")
	}

	decoder := bin.NewBinDecoder(epochScheduleAcct.Data)
	var epochSchedule sealevel.SysvarEpochSchedule
	err = epochSchedule.UnmarshalWithDecoder(decoder)
	if err != nil {
		panic(fmt.Sprintf("unable to unmarshal epoch schedule sysvar when updating clock sysvar"))
	}

	clock.UnixTimestamp = block.UnixTimestamp
	clock.Slot = block.Slot

	epochOld := clock.Epoch
	epochNew := block.Epoch
	clock.Epoch = epochNew

	if epochOld != epochNew {
		clock.EpochStartTimestamp = clock.UnixTimestamp
		clock.LeaderScheduleEpoch = epochSchedule.LeaderScheduleEpoch(clock.Slot)
	}

	return nil
}

func collectAndUpdateSysvarAcctsForAdh(slotCtx *sealevel.SlotCtx) []*accounts.Account {
	sysvarPubkeys := []solana.PublicKey{sealevel.SysvarClockAddr, sealevel.SysvarRecentBlockHashesAddr, sealevel.SysvarSlotHashesAddr, sealevel.SysvarSlotHistoryAddr}
	var sysvarAccts []*accounts.Account

	for _, pk := range sysvarPubkeys {
		acct, err := slotCtx.GetAccount(pk)
		if err != nil {
			panic(fmt.Sprintf("unable to get sysvar account for ADH: %s", pk))
		}

		if acct.Key == sealevel.SysvarRecentBlockHashesAddr {
			decoder := bin.NewBinDecoder(acct.Data)
			var recentBlockhashes sealevel.SysvarRecentBlockhashes

			err = recentBlockhashes.UnmarshalWithDecoder(decoder)
			if err != nil {
				panic(fmt.Sprintf("unable to unmarshal recent blockhashes sysvar"))
			}

			recentBlockhashes.PushLatest(slotCtx.Blockhash)
			newRecentBlockhashesBytes := recentBlockhashes.MustMarshal()
			copy(acct.Data, newRecentBlockhashesBytes)
		} else if acct.Key == sealevel.SysvarSlotHistoryAddr {
			decoder := bin.NewBinDecoder(acct.Data)
			var slotHistory sealevel.SysvarSlotHistory

			err = slotHistory.UnmarshalWithDecoder(decoder)
			if err != nil {
				panic(fmt.Sprintf("unable to unmarshal SlotHistory sysvar"))
			}

			slotHistory.Add(slotCtx.Slot)
			slotHistory.SetNextSlot(slotCtx.Slot + 1)
			newSlotHistoryBytes := slotHistory.MustMarshal()
			copy(acct.Data, newSlotHistoryBytes)
		}
		sysvarAccts = append(sysvarAccts, acct)
	}
	return sysvarAccts
}
