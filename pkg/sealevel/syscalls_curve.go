package sealevel

import (
	"bytes"
	"fmt"
	"math/big"

	"filippo.io/edwards25519"
	"github.com/Overclock-Validator/bgls/curves"
	"github.com/Overclock-Validator/gnark-crypto/ecc/bn254"
	"github.com/Overclock-Validator/mithril/pkg/cu"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/mlog"

	//"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sbpf"
	"github.com/gtank/ristretto255"
	bn256 "github.com/smcio/go-ethereum/crypto/bn256/cloudflare"
)

// curve types
const (
	Curve25519Edwards   = 0
	Curve25519Ristretto = 1
)

const (
	CurvePointBytesLen  = 32
	CurveScalarBytesLen = 32
)

// curve operations
const (
	CurveOpAdd = 0
	CurveOpSub = 1
	CurveOpMul = 2
)

// alt bn128 compression operations
const (
	AltBn128G1Compress   = 0
	AltBn128G1Decompress = 1
	AltBn128G2Compress   = 2
	AltBn128G2Decompress = 3
)

const (
	Bn128G1Len           = 64
	Bn128G2Len           = 128
	Bn128G1CompressedLen = 32
	Bn128G2CompressedLen = 64
)

// alt bn128 operations
const (
	AltBn128Add     = 0
	AltBn128Sub     = 1
	AltBn128Mul     = 2
	AltBn128Pairing = 3
)

// alt bn128 input/output lengths
const (
	AltBn128AdditionInputLen        = 128
	AltBn128MultiplicationInputLen  = 128
	AltBn128PairingElementLen       = 192
	AltBn128AdditionOutputLen       = 64
	AltBn128MultiplicationOutputLen = 64
	AltBn128PairingOutputLen        = 32
)

func SyscallCurveValidatePointImpl(vm sbpf.VM, curveId, pointAddr uint64) (uint64, error) {
	//mlog.Log.Debugf("SyscallCurveValidatePoint")

	execCtx := executionCtx(vm)

	switch curveId {
	case Curve25519Edwards:
		{
			err := execCtx.ComputeMeter.Consume(cu.CUCurve25519EdwardsValidatePointCost)
			if err != nil {
				return syscallCuErr()
			}

			pointBytes, err := vm.Translate(pointAddr, CurvePointBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			var point edwards25519.Point
			_, err = point.SetBytes(pointBytes)
			if err != nil {
				return syscallSuccess(1)
			} else {
				return syscallSuccess(0)
			}
		}

	case Curve25519Ristretto:
		{
			err := execCtx.ComputeMeter.Consume(cu.CUCurve25519RistrettoValidatePointCost)
			if err != nil {
				return syscallCuErr()
			}

			pointBytes, err := vm.Translate(pointAddr, CurvePointBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			err = ristretto255.NewElement().Decode(pointBytes)
			if err != nil {
				return syscallSuccess(1)
			} else {
				return syscallSuccess(0)
			}
		}

	default:
		{
			if execCtx.Features.IsActive(features.AbortOnInvalidCurve) {
				return syscallErrCustom("SyscallError::InvalidAttribute")
			} else {
				return syscallSuccess(1)
			}
		}
	}
}

var SyscallValidatePoint = sbpf.SyscallFunc2(SyscallCurveValidatePointImpl)

func unmarshalEdwardsScalars(scalarsBytes []byte) ([]*edwards25519.Scalar, error) {
	scalars := make([]*edwards25519.Scalar, 0)
	reader := bytes.NewReader(scalarsBytes)

	for count := 0; count < len(scalarsBytes)/CurveScalarBytesLen; count++ {
		scalarBuf := make([]byte, CurveScalarBytesLen)

		n, err := reader.Read(scalarBuf)
		if n != CurveScalarBytesLen || err != nil {
			return nil, fmt.Errorf("not enough bytes deserializing scalars")
		}

		var scalar edwards25519.Scalar
		_, err = scalar.SetCanonicalBytes(scalarBuf)
		if err != nil {
			return nil, fmt.Errorf("error deserializing scalars")
		}

		scalars = append(scalars, &scalar)
	}

	return scalars, nil
}

func unmarshalEdwardsPoints(pointsBytes []byte) ([]*edwards25519.Point, error) {
	points := make([]*edwards25519.Point, 0)
	reader := bytes.NewReader(pointsBytes)

	for count := 0; count < len(pointsBytes)/CurvePointBytesLen; count++ {
		pointBuf := make([]byte, CurvePointBytesLen)

		n, err := reader.Read(pointBuf)
		if n != CurvePointBytesLen || err != nil {
			return nil, fmt.Errorf("not enough bytes deserializing points")
		}

		var point edwards25519.Point
		_, err = point.SetBytes(pointBuf)
		if err != nil {
			return nil, fmt.Errorf("error deserializing points")
		}

		points = append(points, &point)
	}

	return points, nil
}

func unmarshalRistrettoScalars(scalarsBytes []byte) ([]*ristretto255.Scalar, error) {
	scalars := make([]*ristretto255.Scalar, 0)
	reader := bytes.NewReader(scalarsBytes)

	for count := 0; count < len(scalarsBytes)/CurveScalarBytesLen; count++ {
		scalarBuf := make([]byte, CurveScalarBytesLen)

		n, err := reader.Read(scalarBuf)
		if n != CurveScalarBytesLen || err != nil {
			return nil, fmt.Errorf("not enough bytes deserializing scalars")
		}

		scalar := ristretto255.NewScalar()
		err = scalar.Decode(scalarBuf)
		if err != nil {
			return nil, fmt.Errorf("error deserializing scalars")
		}

		scalars = append(scalars, scalar)
	}

	return scalars, nil
}

func unmarshalRistrettoElements(elementsBytes []byte) ([]*ristretto255.Element, error) {
	elements := make([]*ristretto255.Element, 0)
	reader := bytes.NewReader(elementsBytes)

	for count := 0; count < len(elementsBytes)/CurvePointBytesLen; count++ {
		elementBuf := make([]byte, CurvePointBytesLen)

		n, err := reader.Read(elementBuf)
		if n != CurvePointBytesLen || err != nil {
			return nil, fmt.Errorf("not enough bytes deserializing element")
		}

		element := ristretto255.NewElement()
		err = element.Decode(elementBuf)
		if err != nil {
			return nil, fmt.Errorf("error deserializing ristretto element. %s", err)
		}

		elements = append(elements, element)
	}

	return elements, nil
}

func SyscallCurveMultiscalarMultiplicationImpl(vm sbpf.VM, curveId, scalarsAddr, pointsAddr, pointsLen, resultPointAddr uint64) (uint64, error) {
	//mlog.Log.Debugf("SyscallCurveMultiscalarMultiplication")

	execCtx := executionCtx(vm)

	if pointsLen > 512 {
		return syscallErrCustom("SyscallError::InvalidLength")
	}

	switch curveId {
	case Curve25519Edwards:
		{
			cost := cu.CUCurve25519EdwardsMsmBaseCost + (cu.CUCurve25519EdwardsMsmIncrementalCost * (safemath.SaturatingSubU64(pointsLen, 1)))
			err := execCtx.ComputeMeter.Consume(cost)
			if err != nil {
				return syscallCuErr()
			}

			scalarsBytes, err := vm.Translate(scalarsAddr, pointsLen*CurveScalarBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			scalars, err := unmarshalEdwardsScalars(scalarsBytes)
			if err != nil {
				return syscallErr(err)
			}

			pointsBytes, err := vm.Translate(pointsAddr, pointsLen*CurvePointBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			points, err := unmarshalEdwardsPoints(pointsBytes)
			if err != nil {
				return syscallErr(err)
			}

			resultPoint := edwards25519.NewIdentityPoint()
			resultPoint.MultiScalarMult(scalars, points)

			resultSlice, err := vm.Translate(resultPointAddr, CurvePointBytesLen, true)
			if err != nil {
				return syscallErr(err)
			}

			copy(resultSlice, resultPoint.Bytes())

			return syscallSuccess(0)
		}

	case Curve25519Ristretto:
		{
			cost := cu.CUCurve25519RistrettoMsmBaseCost + (cu.CUCurve25519RistrettoMsmIncrementalCost * (safemath.SaturatingSubU64(pointsLen, 1)))
			err := execCtx.ComputeMeter.Consume(cost)
			if err != nil {
				return syscallCuErr()
			}

			scalarsBytes, err := vm.Translate(scalarsAddr, pointsLen*CurveScalarBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			scalars, err := unmarshalRistrettoScalars(scalarsBytes)
			if err != nil {
				return syscallErr(err)
			}

			pointsBytes, err := vm.Translate(pointsAddr, pointsLen*CurvePointBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			points, err := unmarshalRistrettoElements(pointsBytes)
			if err != nil {
				return syscallErr(err)
			}

			resultPoint := ristretto255.NewElement().MultiScalarMult(scalars, points)

			resultSlice, err := vm.Translate(resultPointAddr, CurvePointBytesLen, true)
			if err != nil {
				return syscallErr(err)
			}

			copy(resultSlice, resultPoint.Encode([]byte{}))
			return syscallSuccess(0)
		}

	default:
		{
			if execCtx.Features.IsActive(features.AbortOnInvalidCurve) {
				return syscallErrCustom("SyscallError::InvalidAttribute")
			} else {
				return syscallSuccess(1)
			}
		}
	}
}

var SyscallCurveMultiscalarMultiplication = sbpf.SyscallFunc5(SyscallCurveMultiscalarMultiplicationImpl)

func handleEdwardsCurveGroupOps(vm sbpf.VM, groupOp, leftInputAddr, rightInputAddr, resultPointAddr uint64) (uint64, error) {
	execCtx := executionCtx(vm)

	switch groupOp {
	case CurveOpAdd:
		{
			err := execCtx.ComputeMeter.Consume(cu.CUCurve25519EdwardsAddCost)
			if err != nil {
				return syscallCuErr()
			}

			leftPointBytes, err := vm.Translate(leftInputAddr, CurvePointBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			rightPointBytes, err := vm.Translate(rightInputAddr, CurvePointBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			leftPoint, err := new(edwards25519.Point).SetBytes(leftPointBytes)
			if err != nil {
				return syscallSuccess(1)
			}

			rightPoint, err := new(edwards25519.Point).SetBytes(rightPointBytes)
			if err != nil {
				return syscallSuccess(1)
			}

			resultPoint := new(edwards25519.Point).Add(leftPoint, rightPoint)

			resultPointSlice, err := vm.Translate(resultPointAddr, CurvePointBytesLen, true)
			if err != nil {
				return syscallErr(err)
			}

			copy(resultPointSlice, resultPoint.Bytes())
			return syscallSuccess(0)
		}

	case CurveOpSub:
		{
			err := execCtx.ComputeMeter.Consume(cu.CUCurve25519EdwardsSubCost)
			if err != nil {
				return syscallErr(err)
			}

			leftPointBytes, err := vm.Translate(leftInputAddr, CurvePointBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			rightPointBytes, err := vm.Translate(rightInputAddr, CurvePointBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			leftPoint, err := new(edwards25519.Point).SetBytes(leftPointBytes)
			if err != nil {
				return syscallSuccess(1)
			}

			rightPoint, err := new(edwards25519.Point).SetBytes(rightPointBytes)
			if err != nil {
				return syscallSuccess(1)
			}

			resultPoint := new(edwards25519.Point).Subtract(leftPoint, rightPoint)

			resultPointSlice, err := vm.Translate(resultPointAddr, CurvePointBytesLen, true)
			if err != nil {
				return syscallErr(err)
			}

			copy(resultPointSlice, resultPoint.Bytes())
			return syscallSuccess(0)
		}

	case CurveOpMul:
		{
			err := execCtx.ComputeMeter.Consume(cu.CUCurve25519EdwardsMulCost)
			if err != nil {
				return syscallErr(err)
			}

			scalarBytes, err := vm.Translate(leftInputAddr, CurveScalarBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			inputPointBytes, err := vm.Translate(rightInputAddr, CurvePointBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			scalar, err := new(edwards25519.Scalar).SetCanonicalBytes(scalarBytes)
			if err != nil {
				return syscallSuccess(1)
			}

			inputPoint, err := new(edwards25519.Point).SetBytes(inputPointBytes)
			if err != nil {
				return syscallSuccess(1)
			}

			resultPoint := new(edwards25519.Point).ScalarMult(scalar, inputPoint)

			resultPointSlice, err := vm.Translate(resultPointAddr, CurvePointBytesLen, true)
			if err != nil {
				return syscallErr(err)
			}

			copy(resultPointSlice, resultPoint.Bytes())
			return syscallSuccess(0)
		}

	default:
		{
			if execCtx.Features.IsActive(features.AbortOnInvalidCurve) {
				return syscallErrCustom("SyscallError::InvalidAttribute")
			} else {
				return syscallSuccess(1)
			}
		}
	}
}

func handleRistrettoCurveGroupOps(vm sbpf.VM, groupOp, leftInputAddr, rightInputAddr, resultPointAddr uint64) (uint64, error) {
	execCtx := executionCtx(vm)

	switch groupOp {
	case CurveOpAdd:
		{
			err := execCtx.ComputeMeter.Consume(cu.CUCurve25519RistrettoAddCost)
			if err != nil {
				return syscallCuErr()
			}

			leftPointBytes, err := vm.Translate(leftInputAddr, CurvePointBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			rightPointBytes, err := vm.Translate(rightInputAddr, CurvePointBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			leftPoint := ristretto255.NewElement()
			err = leftPoint.Decode(leftPointBytes)
			if err != nil {
				return syscallSuccess(1)
			}

			rightPoint := ristretto255.NewElement()
			err = rightPoint.Decode(rightPointBytes)
			if err != nil {
				return syscallSuccess(1)
			}

			resultPoint := new(ristretto255.Element).Add(leftPoint, rightPoint)

			resultPointSlice, err := vm.Translate(resultPointAddr, CurvePointBytesLen, true)
			if err != nil {
				return syscallErr(err)
			}

			copy(resultPointSlice, resultPoint.Encode([]byte{}))
			return syscallSuccess(0)
		}

	case CurveOpSub:
		{
			err := execCtx.ComputeMeter.Consume(cu.CUCurve25519RistrettoSubCost)
			if err != nil {
				return syscallCuErr()
			}

			leftPointBytes, err := vm.Translate(leftInputAddr, CurvePointBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			rightPointBytes, err := vm.Translate(rightInputAddr, CurvePointBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			var leftPoint ristretto255.Element
			err = leftPoint.Decode(leftPointBytes)
			if err != nil {
				return syscallSuccess(1)
			}

			var rightPoint ristretto255.Element
			err = rightPoint.Decode(rightPointBytes)
			if err != nil {
				return syscallSuccess(1)
			}

			resultPoint := new(ristretto255.Element).Subtract(&leftPoint, &rightPoint)

			resultPointSlice, err := vm.Translate(resultPointAddr, CurvePointBytesLen, true)
			if err != nil {
				return syscallErr(err)
			}

			copy(resultPointSlice, resultPoint.Encode([]byte{}))
			return syscallSuccess(0)
		}

	case CurveOpMul:
		{
			err := execCtx.ComputeMeter.Consume(cu.CUCurve25519RistrettoAddCost)
			if err != nil {
				return syscallCuErr()
			}

			scalarBytes, err := vm.Translate(leftInputAddr, CurveScalarBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			inputPointBytes, err := vm.Translate(rightInputAddr, CurvePointBytesLen, false)
			if err != nil {
				return syscallErr(err)
			}

			scalar := ristretto255.NewScalar()
			err = scalar.Decode(scalarBytes)
			if err != nil {
				return syscallSuccess(1)
			}

			element := ristretto255.NewElement()
			err = element.Decode(inputPointBytes)
			if err != nil {
				return syscallSuccess(1)
			}

			result := ristretto255.NewElement().ScalarMult(scalar, element)

			resultPointSlice, err := vm.Translate(resultPointAddr, CurvePointBytesLen, true)
			if err != nil {
				return syscallErr(err)
			}
			copy(resultPointSlice, result.Encode([]byte{}))

			return syscallSuccess(0)
		}

	default:
		{
			if execCtx.Features.IsActive(features.AbortOnInvalidCurve) {
				return syscallErrCustom("SyscallError::InvalidAttribute")
			} else {
				return syscallSuccess(1)
			}
		}
	}
}

func SyscallCurveGroupOpsImpl(vm sbpf.VM, curveId, groupOp, leftInputAddr, rightInputAddr, resultPointAddr uint64) (uint64, error) {
	//mlog.Log.Debugf("SyscallCurveGroupOps")

	switch curveId {
	case Curve25519Edwards:
		{
			return handleEdwardsCurveGroupOps(vm, groupOp, leftInputAddr, rightInputAddr, resultPointAddr)
		}

	case Curve25519Ristretto:
		{
			return handleRistrettoCurveGroupOps(vm, groupOp, leftInputAddr, rightInputAddr, resultPointAddr)
		}

	default:
		{
			execCtx := executionCtx(vm)
			if execCtx.Features.IsActive(features.AbortOnInvalidCurve) {
				return syscallErrCustom("SyscallError::InvalidAttribute")
			} else {
				return syscallSuccess(1)
			}
		}
	}
}

var SyscallCurveGroupOps = sbpf.SyscallFunc5(SyscallCurveGroupOpsImpl)

var empty32Bytes [32]byte
var empty64Bytes [64]byte
var empty128Bytes [128]byte

func g1Compress(input []byte) ([]byte, error) {
	if len(input) != Bn128G1Len {
		return nil, fmt.Errorf("wrong input length")
	}

	altbn128 := curves.Altbn128
	point, success := altbn128.UnmarshalG1(input, false)
	if !success {
		return nil, fmt.Errorf("error unmarshaling G1 point")
	}

	compressedPointBytes := point.Marshal()
	return compressedPointBytes, nil
}

func g1Decompress(input []byte) ([]byte, error) {
	if len(input) != Bn128G1CompressedLen {
		return nil, fmt.Errorf("wrong input length")
	}

	if bytes.Compare(input, empty32Bytes[:]) == 0 {
		return empty64Bytes[:], nil
	}

	var g1 bn254.G1Affine
	err := g1.Unmarshal(input)
	if err != nil {
		return nil, err
	}

	decompressedPointBytes := g1.RawBytes()
	return decompressedPointBytes[:], nil
}

func g2Compress(input []byte) ([]byte, error) {
	if len(input) != Bn128G2Len {
		return nil, fmt.Errorf("wrong input length")
	}

	altbn128 := curves.Altbn128
	point, success := altbn128.UnmarshalG2(input, false)
	if !success {
		return nil, fmt.Errorf("error unmarshaling G2 point")
	}

	compressedPointBytes := point.Marshal()
	return compressedPointBytes, nil
}

func g2Decompress(input []byte) ([]byte, error) {
	if len(input) != Bn128G2CompressedLen {
		return nil, fmt.Errorf("wrong input length")
	}

	if bytes.Compare(input, empty64Bytes[:]) == 0 {
		return empty128Bytes[:], nil
	}

	var g2 bn254.G2Affine
	err := g2.Unmarshal(input)
	if err != nil {
		return nil, err
	}

	decompressedBytes := g2.RawBytes()
	return decompressedBytes[:], nil
}

func SyscallAltBn128CompressionImpl(vm sbpf.VM, op, inputAddr, inputLen, resultAddr uint64) (uint64, error) {
	//mlog.Log.Debugf("SyscallAltBn128Compression")

	var cost uint64
	var outputLen uint64

	switch op {
	case AltBn128G1Compress:
		{
			cost = cu.CUSyscallBaseCost + cu.CUBn128G1Compress
			outputLen = Bn128G1CompressedLen
		}

	case AltBn128G1Decompress:
		{
			cost = cu.CUSyscallBaseCost + cu.CUBn128G1Decompress
			outputLen = Bn128G1Len
		}

	case AltBn128G2Compress:
		{
			cost = cu.CUSyscallBaseCost + cu.CUBn128G2Compress
			outputLen = Bn128G2CompressedLen
		}

	case AltBn128G2Decompress:
		{
			cost = cu.CUSyscallBaseCost + cu.CUBn128G2Decompress
			outputLen = Bn128G2Len
		}

	default:
		{
			return syscallErrCustom("SyscallError::InvalidAttribute")
		}
	}

	execCtx := executionCtx(vm)
	err := execCtx.ComputeMeter.Consume(cost)
	if err != nil {
		return syscallCuErr()
	}

	inputSlice, err := vm.Translate(inputAddr, inputLen, false)
	if err != nil {
		return syscallErr(err)
	}

	callResult, err := vm.Translate(resultAddr, outputLen, true)
	if err != nil {
		return syscallErr(err)
	}

	switch op {
	case AltBn128G1Compress:
		{
			//mlog.Log.Debugf("AltBn128G1Compress")

			compressedPointBytes, err := g1Compress(inputSlice)
			if err != nil {
				//mlog.Log.Debugf("G1 compress error: %s", err)
				return syscallSuccess(1)
			}

			copy(callResult, compressedPointBytes)
			return syscallSuccess(0)
		}

	case AltBn128G1Decompress:
		{
			//mlog.Log.Debugf("AltBn128G1Decompress")

			decompressedPointBytes, err := g1Decompress(inputSlice)
			if err != nil {
				//mlog.Log.Debugf("G1 decompress error: %s", err)
				return syscallSuccess(1)
			}

			copy(callResult, decompressedPointBytes)
			return syscallSuccess(0)
		}

	case AltBn128G2Compress:
		{
			//mlog.Log.Debugf("AltBn128G2Compress")

			compressedPointBytes, err := g2Compress(inputSlice)
			if err != nil {
				//mlog.Log.Debugf("G2 compress error: %s", err)
				return syscallSuccess(1)
			}

			copy(callResult, compressedPointBytes)
			return syscallSuccess(0)
		}

	case AltBn128G2Decompress:
		{
			//mlog.Log.Debugf("AltBn128G2Decompress")

			decompressedPointBytes, err := g2Decompress(inputSlice)
			if err != nil {
				//mlog.Log.Debugf("G2 decompress error: %s", err)
				return syscallSuccess(1)
			}

			copy(callResult, decompressedPointBytes)
			return syscallSuccess(0)
		}

	default:
		{
			return syscallErrCustom("SyscallError::InvalidAttribute")
		}
	}
}

var SyscallAltBn128Compression = sbpf.SyscallFunc4(SyscallAltBn128CompressionImpl)

func altbn128Addition(input []byte) ([]byte, error) {
	if len(input) > AltBn128AdditionInputLen {
		return nil, fmt.Errorf("AltBn128Error::InvalidInputData")
	}

	paddedInput := make([]byte, AltBn128AdditionInputLen)
	copy(paddedInput, input)
	input = paddedInput

	point1 := new(bn256.G1)
	point2 := new(bn256.G1)

	_, err := point1.Unmarshal(input[:64])
	if err != nil {
		return nil, err
	}

	_, err = point2.Unmarshal(input[64:AltBn128AdditionInputLen])
	if err != nil {
		return nil, err
	}

	resultPoint := new(bn256.G1)
	resultPoint.Add(point1, point2)
	resultBytes := resultPoint.Marshal()

	return resultBytes, nil
}

func altbn128Multiplication(input []byte, expectedLen uint64) ([]byte, error) {
	if uint64(len(input)) > expectedLen {
		return nil, fmt.Errorf("AltBn128Error::InvalidInputData")
	}

	paddedInput := make([]byte, 96)
	copy(paddedInput, input)

	point := new(bn256.G1)
	_, err := point.Unmarshal(paddedInput[:64])
	if err != nil {
		return nil, err
	}

	scalar := new(big.Int).SetBytes(paddedInput[64:96])
	resultPoint := new(bn256.G1)
	resultPoint.ScalarMult(point, scalar)
	resultBytes := resultPoint.Marshal()

	return resultBytes, nil
}

func altbn128Pairing(input []byte) ([]byte, error) {
	elementsLen := uint64(len(input)) / AltBn128PairingElementLen

	g1Vals := make([]*bn256.G1, 0)
	g2Vals := make([]*bn256.G2, 0)

	for count := uint64(0); count < elementsLen; count++ {
		point1 := new(bn256.G1)
		point2 := new(bn256.G2)

		_, err := point1.Unmarshal(input[count*192 : (count*192)+64])
		if err != nil {
			return nil, err
		}

		_, err = point2.Unmarshal(input[(count*192)+64 : (count*192)+64+128])
		if err != nil {
			return nil, err
		}

		g1Vals = append(g1Vals, point1)
		g2Vals = append(g2Vals, point2)
	}

	var isPaired bool
	if len(g1Vals) != 0 {
		isPaired = bn256.PairingCheck(g1Vals, g2Vals)
	}

	var callResult [32]byte

	if isPaired || len(g1Vals) == 0 {
		callResult[31] = 1
	} else {
		mlog.Log.Debugf("PairingCheck fail\n")
	}

	return callResult[:], nil
}

func SyscallAltBn128Impl(vm sbpf.VM, groupOp, inputAddr, inputLen, resultAddr uint64) (uint64, error) {
	//mlog.Log.Debugf("SyscallAltBn128")

	var cost uint64
	var outputLen uint64

	switch groupOp {
	case AltBn128Add:
		{
			//mlog.Log.Debugf("AltBn128Add. inputLen = %d", inputLen)
			cost = cu.CUBn128AdditionCost
			outputLen = AltBn128AdditionOutputLen
		}

	case AltBn128Mul:
		{
			//mlog.Log.Debugf("AltBn128Mul. inputLen = %d", inputLen)
			cost = cu.CUBn128MultiplicationCost
			outputLen = AltBn128MultiplicationOutputLen
		}

	case AltBn128Pairing:
		{
			//mlog.Log.Debugf("AltBn128Pairing. inputLen = %d", inputLen)
			elementLen := inputLen / AltBn128PairingElementLen
			cost = cu.CUBn128PairingOnePairCostFirst + cu.CUSha256BaseCost + AltBn128PairingOutputLen
			cost = safemath.SaturatingAddU64(cost, safemath.SaturatingMulU64(cu.CUBn128PairingOnePairCostOther, safemath.SaturatingSubU64(elementLen, 1)))
			cost = safemath.SaturatingAddU64(cost, inputLen)
			outputLen = AltBn128PairingOutputLen
		}

	default:
		{
			return syscallErrCustom("SyscallError::InvalidAttribute")
		}
	}

	execCtx := executionCtx(vm)
	err := execCtx.ComputeMeter.Consume(cost)
	if err != nil {
		return syscallCuErr()
	}

	inputSlice, err := vm.Translate(inputAddr, inputLen, false)
	if err != nil {
		return syscallErr(err)
	}

	callResult, err := vm.Translate(resultAddr, outputLen, true)
	if err != nil {
		return syscallErr(err)
	}

	switch groupOp {
	case AltBn128Add:
		{
			result, err := altbn128Addition(inputSlice)
			if err != nil {
				mlog.Log.Debugf("altbn128 addition err: %s", err)
				return syscallSuccess(1)
			} else {
				copy(callResult, result)
				return syscallSuccess(0)
			}
		}

	case AltBn128Mul:
		{
			var expectedSize uint64
			if execCtx.Features.IsActive(features.FixAltBn128MultiplicationInputLength) {
				expectedSize = 96
			} else {
				expectedSize = 128
			}

			result, err := altbn128Multiplication(inputSlice, expectedSize)
			if err != nil {
				mlog.Log.Debugf("altbn128 multiplication err: %s", err)
				return syscallSuccess(1)
			} else {
				copy(callResult, result)
				return syscallSuccess(0)
			}
		}

	case AltBn128Pairing:
		{
			result, err := altbn128Pairing(inputSlice)
			if err != nil {
				mlog.Log.Debugf("altbn128 pairing err: %s", err)
				return syscallSuccess(1)
			} else {
				copy(callResult, result)
				return syscallSuccess(0)
			}
		}

	default:
		{
			return syscallErrCustom("SyscallError::InvalidAttribute")
		}
	}
}

var SyscallAltBn128 = sbpf.SyscallFunc4(SyscallAltBn128Impl)
