package rpcclient

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go/rpc"
)

// AlpenglowFooterRPC is the optional footer payload returned by getBlockAG.
type AlpenglowFooterRPC struct {
	BankHash               string  `json:"bankHash"`
	BlockProducerTimeNanos uint64  `json:"blockProducerTimeNanos"`
	FinalCert              *string `json:"finalCert,omitempty"`
	SkipRewardCert         *string `json:"skipRewardCert,omitempty"`
	NotarRewardCert        *string `json:"notarRewardCert,omitempty"`
}

// EpochCreditsEntryRPC is a tail element from a vote account epoch credits vector.
type EpochCreditsEntryRPC struct {
	Epoch       uint64 `json:"epoch"`
	Credits     uint64 `json:"credits"`
	PrevCredits uint64 `json:"prevCredits"`
}

// LeaderVoteReplayDiagRPC captures leader vote state after Agave bank replay.
type LeaderVoteReplayDiagRPC struct {
	VoteAccount       string                 `json:"voteAccount"`
	Lamports          uint64                 `json:"lamports"`
	RootSlot          *uint64                `json:"rootSlot,omitempty"`
	LastVotedSlot     *uint64                `json:"lastVotedSlot,omitempty"`
	EpochCreditsTail  []EpochCreditsEntryRPC `json:"epochCreditsTail,omitempty"`
}

// ReplayDiagRPC mirrors Mithril footer bank hash mismatch logs for side-by-side diff.
type ReplayDiagRPC struct {
	Slot                       uint64                   `json:"slot"`
	ParentSlot                 uint64                   `json:"parentSlot"`
	BankFrozen                 bool                     `json:"bankFrozen"`
	ComputedBankHash           *string                  `json:"computedBankHash,omitempty"`
	ParentBankHash             string                   `json:"parentBankHash"`
	LastBlockhash              string                   `json:"lastBlockhash"`
	SignatureCount             uint64                   `json:"signatureCount"`
	AccountsLtHashChecksum     string                   `json:"accountsLtHashChecksum"`
	ClockUnixTimestamp         int64                    `json:"clockUnixTimestamp"`
	ClockEpoch                 uint64                   `json:"clockEpoch"`
	ClockEpochStartTimestamp   int64                    `json:"clockEpochStartTimestamp"`
	ClockLeaderScheduleEpoch   uint64                   `json:"clockLeaderScheduleEpoch"`
	NanosecondClock            *int64                   `json:"nanosecondClock,omitempty"`
	Leader                     string                   `json:"leader"`
	LeaderVoteAccount          string                   `json:"leaderVoteAccount"`
	BankExpectedHash           *string                  `json:"bankExpectedHash,omitempty"`
	FooterBankHash             *string                  `json:"footerBankHash,omitempty"`
	FooterMatchesComputed      *bool                    `json:"footerMatchesComputed,omitempty"`
	FooterMatchesBankExpected  *bool                    `json:"footerMatchesBankExpected,omitempty"`
	RewardSlot                 *uint64                  `json:"rewardSlot,omitempty"`
	RewardEpoch                *uint64                  `json:"rewardEpoch,omitempty"`
	RewardEpochTotalStake      *uint64                  `json:"rewardEpochTotalStake,omitempty"`
	TransactionCount           *uint64                  `json:"transactionCount,omitempty"`
	SkipRewardCertLen          *int                     `json:"skipRewardCertLen,omitempty"`
	NotarRewardCertLen         *int                     `json:"notarRewardCertLen,omitempty"`
	FinalCertLen               *int                     `json:"finalCertLen,omitempty"`
	LeaderVote                 *LeaderVoteReplayDiagRPC `json:"leaderVote,omitempty"`
}

// GetBlockAGResult is the getBlockAG response: getBlock fields plus optional Alpenglow footer.
type GetBlockAGResult struct {
	rpc.GetBlockResult
	AlpenglowFooter *AlpenglowFooterRPC `json:"alpenglowFooter,omitempty"`
	ReplayDiag      *ReplayDiagRPC      `json:"replayDiag,omitempty"`
}

func getBlockAGOpts() (map[string]interface{}, uint64) {
	includeRewards := true
	maxSupportedTxVer := uint64(0)
	return map[string]interface{}{
		"maxSupportedTransactionVersion": maxSupportedTxVer,
		"commitment":                     rpc.CommitmentConfirmed,
		"transactionDetails":             rpc.TransactionDetailsFull,
		"rewards":                        includeRewards,
	}, maxSupportedTxVer
}

// GetBlockAGConfirmedOnce fetches a block with Alpenglow footer metadata via getBlockAG.
func (fetcher *RpcClient) GetBlockAGConfirmedOnce(slot uint64) (*GetBlockAGResult, error) {
	opts, _ := getBlockAGOpts()
	params := []interface{}{slot, opts}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result GetBlockAGResult
	err := fetcher.client.RPCCallForInto(ctx, &result, "getBlockAG", params)
	if err != nil {
		if strings.Contains(err.Error(), fmt.Sprintf("Slot %d was skipped", slot)) {
			return nil, SlotSkipped
		}
		return nil, err
	}

	return &result, nil
}

// DecodeAlpenglowFooterCerts decodes base64 footer cert fields into raw wincode bytes.
func DecodeAlpenglowFooterCerts(footer *AlpenglowFooterRPC) (skipCert, notarCert, finalCert []byte, err error) {
	if footer == nil {
		return nil, nil, nil, nil
	}
	if footer.FinalCert != nil && *footer.FinalCert != "" {
		finalCert, err = base64.StdEncoding.DecodeString(*footer.FinalCert)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("decode finalCert: %w", err)
		}
	}
	if footer.SkipRewardCert != nil && *footer.SkipRewardCert != "" {
		skipCert, err = base64.StdEncoding.DecodeString(*footer.SkipRewardCert)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("decode skipRewardCert: %w", err)
		}
	}
	if footer.NotarRewardCert != nil && *footer.NotarRewardCert != "" {
		notarCert, err = base64.StdEncoding.DecodeString(*footer.NotarRewardCert)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("decode notarRewardCert: %w", err)
		}
	}
	return skipCert, notarCert, finalCert, nil
}
