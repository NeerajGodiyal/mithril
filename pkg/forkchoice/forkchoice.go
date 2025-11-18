package forkchoice

import (
	"fmt"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/gagliardetto/solana-go"
	"github.com/panjf2000/ants/v2"
)

type blockJob struct {
	slot uint64
	txs  []*solana.Transaction
}

type voteUpdate struct {
	voteInfo *voteInfo
	stake    uint64
}

type forkChoiceState struct {
	voteStakeTotals       map[uint64]*voteStakeAccumulator
	epoch                 uint64
	epochStakes           map[solana.PublicKey]uint64
	epochAuthorizedVoters *epochstakes.EpochAuthorizedVotersCache
	totalEpochStake       uint64
	slotAlreadyConfirmed  map[uint64]struct{}
	latestSlotIngested    uint64
	mu                    sync.Mutex
}

type ForkChoiceService struct {
	state    *forkChoiceState
	pool     *ants.PoolWithFunc
	jobChan  chan *blockJob
	wg       sync.WaitGroup
	shutdown chan struct{}
}

func NewForkChoiceService(
	epoch uint64,
	epochStakes map[solana.PublicKey]uint64,
	totalEpochStake uint64,
	epochAuthorizedVoters *epochstakes.EpochAuthorizedVotersCache,
	poolSize int,
) (*ForkChoiceService, error) {

	state := &forkChoiceState{
		voteStakeTotals:       make(map[uint64]*voteStakeAccumulator),
		slotAlreadyConfirmed:  make(map[uint64]struct{}),
		epoch:                 epoch,
		epochStakes:           epochStakes,
		epochAuthorizedVoters: epochAuthorizedVoters,
		totalEpochStake:       totalEpochStake,
	}

	service := &ForkChoiceService{
		state:    state,
		jobChan:  make(chan *blockJob, poolSize),
		shutdown: make(chan struct{}),
	}

	poolSubmitFunc := func(job interface{}) {
		service.processBlock(job.(*blockJob))
	}

	pool, err := ants.NewPoolWithFunc(poolSize, poolSubmitFunc)
	if err != nil {
		return nil, fmt.Errorf("failed to create ants pool: %w", err)
	}
	service.pool = pool

	return service, nil
}

func (s *ForkChoiceService) Start() {
	s.wg.Add(1)
	go s.run()
}

func (s *ForkChoiceService) Stop() {
	close(s.shutdown)
	s.wg.Wait()
	s.pool.Release()
	close(s.jobChan)
}

func (s *ForkChoiceService) run() {
	defer s.wg.Done()
	for {
		select {
		case job, ok := <-s.jobChan:
			if !ok {
				return
			}
			if err := s.pool.Invoke(job); err != nil {
				fmt.Printf("error submitting job to ants pool for slot %d: %v\n", job.slot, err)
			}
		case <-s.shutdown:
			return
		}
	}
}

func (s *ForkChoiceService) SubmitBlock(slot uint64, txs []*solana.Transaction) {
	job := &blockJob{
		slot: slot,
		txs:  txs,
	}

	select {
	case s.jobChan <- job:
	case <-s.shutdown:
		fmt.Printf("fork choice service shutting down, discarding job for slot %d\n", slot)
	}
}

func (s *ForkChoiceService) processBlock(job *blockJob) {
	var updatesToApply []voteUpdate

	for _, tx := range job.txs {
		if !tx.IsVote() {
			continue
		}

		voteInfo, ok := s.state.parseAndValidateVoteTxForBankhashAndSlot(tx)
		if !ok {
			continue
		}

		stakeForVoteAcct, ok := s.state.epochStakes[voteInfo.votePubkey]
		if !ok {
			continue
		}

		updatesToApply = append(updatesToApply, voteUpdate{
			voteInfo: voteInfo,
			stake:    stakeForVoteAcct,
		})
	}

	s.state.mu.Lock()
	defer s.state.mu.Unlock()

	for _, update := range updatesToApply {
		_, alreadyConfirmed := s.state.slotAlreadyConfirmed[update.voteInfo.slot]
		if alreadyConfirmed {
			continue
		}

		accumulator, exists := s.state.voteStakeTotals[update.voteInfo.slot]
		if !exists {
			accumulator = newVoteStakeAccumulator(s.state.totalEpochStake, update.voteInfo.slot)
			s.state.voteStakeTotals[update.voteInfo.slot] = accumulator
		}

		accumulator.add(update.voteInfo.bankHash, update.stake)
	}

	if s.state.latestSlotIngested < job.slot {
		s.state.latestSlotIngested = job.slot
	}
}

const (
	BankhashHasSupermajority = iota
	BankhashNoSupermajority
	BankhashNeedWait
)

const voteLandingPeriodInSlots = 32

func (s *ForkChoiceService) IsBankhashCorrect(slot uint64, bankHash solana.Hash) int {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()

	if s.state.latestSlotIngested < (slot + voteLandingPeriodInSlots) {
		return BankhashNeedWait
	}

	accumulator, exists := s.state.voteStakeTotals[slot]
	if !exists {
		return BankhashNeedWait
	}

	if accumulator.hashHasSupermajority(bankHash) {
		s.state.slotAlreadyConfirmed[slot] = struct{}{}
		return BankhashHasSupermajority
	} else {
		return BankhashNoSupermajority
	}
}
