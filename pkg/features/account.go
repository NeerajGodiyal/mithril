package features

import (
	"bytes"

	bin "github.com/gagliardetto/binary"
)

type FeatureAcct struct {
	ActivatedAt *uint64
}

func (featureAcct *FeatureAcct) UnmarshalWithDecoder(decoder *bin.Decoder) error {
	hasActivatedAt, err := decoder.ReadBool()
	if err != nil {
		return err
	}

	if hasActivatedAt {
		activatedAt, err := decoder.ReadUint64(bin.LE)
		if err != nil {
			return err
		}
		featureAcct.ActivatedAt = &activatedAt
	}

	return nil
}

func (featureAcct *FeatureAcct) MarshalWithEncoder(encoder *bin.Encoder) error {
	var err error

	if featureAcct.ActivatedAt != nil {
		err = encoder.WriteBool(true)
		if err != nil {
			return err
		}
		err = encoder.WriteUint64(*featureAcct.ActivatedAt, bin.LE)
		if err != nil {
			return err
		}

		return nil
	} else {
		panic("why are we trying to write out a feature account without an ActivatedAt?")
	}
}

func UnmarshalFeatureAcct(data []byte) *FeatureAcct {
	decoder := bin.NewBinDecoder(data)
	featureAcct := new(FeatureAcct)

	featureAcct.UnmarshalWithDecoder(decoder)
	return featureAcct
}

func MarshalFeatureAcct(featureAcct *FeatureAcct) ([]byte, error) {
	writer := new(bytes.Buffer)
	encoder := bin.NewBinEncoder(writer)

	err := featureAcct.MarshalWithEncoder(encoder)
	if err != nil {
		return nil, err
	}

	return writer.Bytes(), nil
}
