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

var addPayload1 = "18b18acfb4c2c30276db5411368e7185b311dd124691610c5d3b74034e093dc9063c909c4720840cb5134cb9f59fa749755796819658d32efc0d288198f3726607c2b7f58a84bd6145f00c9c2bc0bb1a187f20ff2c92963a88019e7c6a014eed06614e20c147e940f2d70da3f74c9a17df361706a4485c742bd6788478fa17d7"
var addResult1 = "2243525c5efd4b9c3d3c45ac0ca3fe4dd85e830a4ce6b65fa1eeaee202839703301d1d33be6da8e509df21cc35964723180eed7532537db9ae5e7d48f195c915"

var addPayload2 = "2243525c5efd4b9c3d3c45ac0ca3fe4dd85e830a4ce6b65fa1eeaee202839703301d1d33be6da8e509df21cc35964723180eed7532537db9ae5e7d48f195c91518b18acfb4c2c30276db5411368e7185b311dd124691610c5d3b74034e093dc9063c909c4720840cb5134cb9f59fa749755796819658d32efc0d288198f37266"
var addResult2 = "2bd3e6d0f3b142924f5ca7b49ce5b9d54c4703d7ae5648e61d02268b1a0a9fb721611ce0a6af85915e2f1d70300909ce2e49dfad4a4619c8390cae66cefdb204"

var addPayload3 = "17c139df0efee0f766bc0204762b774362e4ded88953a39ce849a8a7fa163fa901e0559bacb160664764a357af8a9fe70baa9258e0b959273ffc5718c6d4cc7c039730ea8dff1254c0fee9c0ea777d29a9c710b7e616683f194f18c43b43b869073a5ffcc6fc7a28c30723d6e58ce577356982d65b833a5a5c15bf9024b43d98"
var addResult3 = "15bf2bb17880144b5d1cd2b1f46eff9d617bffd1ca57c37fb5a49bd84e53cf66049c797f9ce0d17083deb32b5e36f2ea2a212ee036598dd7624c168993d1355f"

func Test_AltBn128_Add1(t *testing.T) {
	stringTestcase := addPayload1
	hexTestcase, err := hex.DecodeString(stringTestcase)
	assert.NoError(t, err)

	result, err := altbn128Addition(hexTestcase)
	assert.NoError(t, err)

	knownCorrectResultBytes, err := hex.DecodeString(addResult1)
	assert.NoError(t, err)

	assert.Equal(t, knownCorrectResultBytes, result)
}

func Test_AltBn128_Add2(t *testing.T) {
	stringTestcase := addPayload2
	hexTestcase, err := hex.DecodeString(stringTestcase)
	assert.NoError(t, err)

	result, err := altbn128Addition(hexTestcase)
	assert.NoError(t, err)

	knownCorrectResultBytes, err := hex.DecodeString(addResult2)
	assert.NoError(t, err)

	assert.Equal(t, knownCorrectResultBytes, result)
}

func Test_AltBn128_Add3(t *testing.T) {
	stringTestcase := addPayload3
	hexTestcase, err := hex.DecodeString(stringTestcase)
	assert.NoError(t, err)

	result, err := altbn128Addition(hexTestcase)
	assert.NoError(t, err)

	knownCorrectResultBytes, err := hex.DecodeString(addResult3)
	assert.NoError(t, err)

	assert.Equal(t, knownCorrectResultBytes, result)
}

var mulPayload1 = "2bd3e6d0f3b142924f5ca7b49ce5b9d54c4703d7ae5648e61d02268b1a0a9fb721611ce0a6af85915e2f1d70300909ce2e49dfad4a4619c8390cae66cefdb20400000000000000000000000000000000000000000000000011138ce750fa15c2"
var mulResult1 = "070a8d6a982153cae4be29d434e8faef8a47b274a053f5a4ee2a6c9c13c31e5c031b8ce914eba3a9ffb989f9cdd5b0f01943074bf4f0f315690ec3cec6981afc"

func Test_AltBn128_Mul1(t *testing.T) {
	stringTestcase := mulPayload1
	hexTestcase, err := hex.DecodeString(stringTestcase)
	assert.NoError(t, err)

	result, err := altbn128Multiplication(hexTestcase, 128)
	assert.NoError(t, err)

	knownCorrectResultBytes, err := hex.DecodeString(mulResult1)
	assert.NoError(t, err)

	assert.Equal(t, knownCorrectResultBytes, result)
}
