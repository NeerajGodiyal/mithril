package rewards

import (
	"github.com/Overclock-Validator/wide"
	"github.com/gagliardetto/solana-go"
)

type CalculatedStakePointsAccumulator struct {
	pubkeys   []solana.PublicKey
	pointsMap map[solana.PublicKey]*CalculatedStakePoints
}

func NewCalculatedStakePointsAccumulator(pubkeys []solana.PublicKey) *CalculatedStakePointsAccumulator {
	stakePointStructs := make([]CalculatedStakePoints, len(pubkeys))
	accum := &CalculatedStakePointsAccumulator{pubkeys: pubkeys, pointsMap: make(map[solana.PublicKey]*CalculatedStakePoints, len(pubkeys))}
	for i, pk := range pubkeys {
		accum.pointsMap[pk] = &stakePointStructs[i]
	}
	return accum
}

func (accum *CalculatedStakePointsAccumulator) Add(pk solana.PublicKey, points CalculatedStakePoints) {
	accum.pointsMap[pk].Points = points.Points
	accum.pointsMap[pk].NewCreditsObserved = points.NewCreditsObserved
	accum.pointsMap[pk].ForceCreditsUpdateWithSkippedReward = points.ForceCreditsUpdateWithSkippedReward
}

func (accum CalculatedStakePointsAccumulator) TotalPoints() wide.Uint128 {
	var totalPoints wide.Uint128
	for _, pk := range accum.pubkeys {
		totalPoints = totalPoints.Add(accum.pointsMap[pk].Points)
	}
	return totalPoints
}

func (accum CalculatedStakePointsAccumulator) CalculatedStakePoints() map[solana.PublicKey]*CalculatedStakePoints {
	return accum.pointsMap
}
