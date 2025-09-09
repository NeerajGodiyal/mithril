package sealevel

import (
	"math"

	bin "github.com/gagliardetto/binary"
)

type FeeRateGovernor struct {
	TargetLamportsPerSignature uint64
	TargetSignaturesPerSlot    uint64
	MinLamportsPerSignature    uint64
	MaxLamportsPerSignature    uint64
	BurnPercent                byte
	LamportsPerSignature       uint64
	PrevLamportsPerSignature   uint64
}

func (rateGovernor *FeeRateGovernor) UnmarshalWithDecoder(decoder *bin.Decoder) error {
	var err error

	rateGovernor.TargetLamportsPerSignature, err = decoder.ReadUint64(bin.LE)
	if err != nil {
		return err
	}

	rateGovernor.TargetSignaturesPerSlot, err = decoder.ReadUint64(bin.LE)
	if err != nil {
		return err
	}

	rateGovernor.MinLamportsPerSignature, err = decoder.ReadUint64(bin.LE)
	if err != nil {
		return err
	}

	rateGovernor.MaxLamportsPerSignature, err = decoder.ReadUint64(bin.LE)
	if err != nil {
		return err
	}

	rateGovernor.BurnPercent, err = decoder.ReadByte()
	return err
}

func NewFeeRateGovernorDerived(baseFeeRateGovernor *FeeRateGovernor, latestSignaturesPerSlot uint64) *FeeRateGovernor {
	me := baseFeeRateGovernor.Clone()

	if me.TargetSignaturesPerSlot > 0 {
		me.MinLamportsPerSignature = max(1, me.TargetLamportsPerSignature/2)
		me.MaxLamportsPerSignature = me.TargetLamportsPerSignature * 10

		desiredLamportsPerSignature := min(
			me.MaxLamportsPerSignature,
			max(
				me.MinLamportsPerSignature,
				me.TargetLamportsPerSignature*min(latestSignaturesPerSlot, math.MaxUint32)/me.TargetSignaturesPerSlot))

		gap := int64(desiredLamportsPerSignature) - int64(baseFeeRateGovernor.LamportsPerSignature)
		if gap == 0 {
			me.LamportsPerSignature = desiredLamportsPerSignature
		} else {
			var multiplier int64

			if gap > 0 {
				multiplier = 1
			} else {
				multiplier = -1
			}

			gapAdjust := int64(max(1, int64(me.TargetLamportsPerSignature/20))) * multiplier
			me.LamportsPerSignature = min(me.MaxLamportsPerSignature, max(me.MinLamportsPerSignature, uint64(int64(baseFeeRateGovernor.LamportsPerSignature)+gapAdjust)))
		}
	} else {
		me.LamportsPerSignature = baseFeeRateGovernor.TargetLamportsPerSignature
		me.MinLamportsPerSignature = me.TargetLamportsPerSignature
		me.MaxLamportsPerSignature = me.TargetLamportsPerSignature
	}

	return me
}

func (rateGovernor *FeeRateGovernor) Clone() *FeeRateGovernor {
	return &FeeRateGovernor{TargetLamportsPerSignature: rateGovernor.TargetLamportsPerSignature,
		TargetSignaturesPerSlot:  rateGovernor.TargetSignaturesPerSlot,
		MinLamportsPerSignature:  rateGovernor.MinLamportsPerSignature,
		MaxLamportsPerSignature:  rateGovernor.MaxLamportsPerSignature,
		BurnPercent:              rateGovernor.BurnPercent,
		PrevLamportsPerSignature: rateGovernor.LamportsPerSignature,
	}
}
