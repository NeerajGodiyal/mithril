package sealevel

import (
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/Overclock-Validator/bgls/curves"
	"github.com/stretchr/testify/assert"
)

var decompressedG1Testcase = "1c76476f4def4bb94541d57ebba1193381ffa7aa76ada664dd31c16024c43f593034dd2920f673e204fee2811c678745fc819b55d3e9d294e45c9b03a76aef41209dd15ebff5d46c4bd888e51a93cf99a7329636c63514396b4a452003a35bf704bf11ca01483bfa8b34b43561848d28905960114c8ac04049af4b6315a416782bb8324af6cfc93537a2ad1a445cfd0ca2a71acd7ac41fadbf933c2a51be344d120a2a4cf30c1bf9845f20c6fe39e07ea2cce61f0c9bb048165fe5e4de877550111e129f1cf1097710d41c4ac70fcdfa5ba2023c6ff1cbeac322de49d1b6df7c2032c61a830e3c17286de9462bf242fca2883585b93870a73853face6a6bf411198e9393920d483a7260bfb731fb5d25f1aa493335a9e71297e485b7aef312c21800deef121f1e76426a00665e5c4479674322d4f75edadd46debd5cd992f6ed090689d0585ff075ec9e99ad690c3395bc4b313370b38ef355acdadcd122975b12c85ea5db8c6deb4aab71808dcb408fe3d1e7690c43d37b4ce6cc0166fa7daa"
var compressedG1Testcase = "9c76476f4def4bb94541d57ebba1193381ffa7aa76ada664dd31c16024c43f59"

var decompressedG2Testcase = "209dd15ebff5d46c4bd888e51a93cf99a7329636c63514396b4a452003a35bf704bf11ca01483bfa8b34b43561848d28905960114c8ac04049af4b6315a416782bb8324af6cfc93537a2ad1a445cfd0ca2a71acd7ac41fadbf933c2a51be344d120a2a4cf30c1bf9845f20c6fe39e07ea2cce61f0c9bb048165fe5e4de877550"
var compressedG2Testcase = "a09dd15ebff5d46c4bd888e51a93cf99a7329636c63514396b4a452003a35bf704bf11ca01483bfa8b34b43561848d28905960114c8ac04049af4b6315a41678"

// test compression of an uncompressed G1 sample
func Test_AltBn128_Compress_G1(t *testing.T) {
	stringTestcase := decompressedG1Testcase[:128]
	hexTestcase, err := hex.DecodeString(stringTestcase)
	assert.NoError(t, err)

	fmt.Printf("G1 compression: decompressesed G1 in:\n%s\n", hex.Dump(hexTestcase))

	altbn128 := curves.Altbn128
	point, success := altbn128.UnmarshalG1(hexTestcase, false)
	assert.Equal(t, success, true)

	compressedBytes := point.Marshal()

	knownCorrectCompressedBytes, err := hex.DecodeString(compressedG1Testcase)
	assert.NoError(t, err)
	assert.Equal(t, knownCorrectCompressedBytes, compressedBytes)

	fmt.Printf("G1 compression: compressed G1 out:\n%s\n", hex.Dump(compressedBytes))
}

// test decompression of a compressed G1 sample
func Test_AltBn128_Decompress_G1(t *testing.T) {
	stringTestcase := compressedG1Testcase[:64]
	hexTestcase, err := hex.DecodeString(stringTestcase)
	assert.NoError(t, err)

	fmt.Printf("G1 decompression: compressed G1;\n%s\n", hex.Dump(hexTestcase))

	altbn128 := curves.Altbn128
	point, success := altbn128.UnmarshalG1(hexTestcase, false)
	assert.Equal(t, success, true)

	decompressedBytes := point.MarshalUncompressed()

	knownCorrectDecompressedBytes, err := hex.DecodeString(decompressedG1Testcase[:128])
	assert.NoError(t, err)
	assert.Equal(t, knownCorrectDecompressedBytes, decompressedBytes)

	fmt.Printf("G1 decompression: decompressed G1 out (%d):\n%s\n", len(decompressedBytes), hex.Dump(decompressedBytes))
}

// test compression of an uncompressed G2 sample
func Test_AltBn128_Compress_G2(t *testing.T) {
	stringTestcase := decompressedG2Testcase
	hexTestcase, err := hex.DecodeString(stringTestcase)
	assert.NoError(t, err)

	fmt.Printf("G2 compression: decompressed G2 in:\n%s\n", hex.Dump(hexTestcase))

	altbn128 := curves.Altbn128
	point, success := altbn128.UnmarshalG2(hexTestcase, false)
	assert.Equal(t, success, true)

	compressedBytes := point.Marshal()

	knownCorrectCompressedBytes, err := hex.DecodeString(compressedG2Testcase)
	assert.NoError(t, err)
	assert.Equal(t, knownCorrectCompressedBytes, compressedBytes)

	fmt.Printf("G2 compression: compressed G2 out:\n%s\n", hex.Dump(compressedBytes))
}

// test decompression of a compressed sample
func Test_AltBn128_Decompress_G2(t *testing.T) {
	stringTestcase := compressedG2Testcase
	hexTestcase, err := hex.DecodeString(stringTestcase)
	assert.NoError(t, err)

	fmt.Printf("G2 decompression: compressed G2 in:\n%s\n", hex.Dump(hexTestcase))

	altbn128 := curves.Altbn128
	point, success := altbn128.UnmarshalG2(hexTestcase, false)
	assert.Equal(t, success, true)

	decompressedBytes := point.MarshalUncompressed()

	knownCorrectDecompressedBytes, err := hex.DecodeString(decompressedG2Testcase)
	assert.NoError(t, err)
	assert.Equal(t, knownCorrectDecompressedBytes, decompressedBytes)

	fmt.Printf("G2 decompression: decompressed G2 out: (%d):\n%s\n", len(decompressedBytes), hex.Dump(decompressedBytes))
}
