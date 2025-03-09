package sealevel

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_AltBn128_Compress_G1(t *testing.T) {
	test, _ := hex.DecodeString("1c76476f4def4bb94541d57ebba1193381ffa7aa76ada664dd31c16024c43f593034dd2920f673e204fee2811c678745fc819b55d3e9d294e45c9b03a76aef41209dd15ebff5d46c4bd888e51a93cf99a7329636c63514396b4a452003a35bf704bf11ca01483bfa8b34b43561848d28905960114c8ac04049af4b6315a416782bb8324af6cfc93537a2ad1a445cfd0ca2a71acd7ac41fadbf933c2a51be344d120a2a4cf30c1bf9845f20c6fe39e07ea2cce61f0c9bb048165fe5e4de877550111e129f1cf1097710d41c4ac70fcdfa5ba2023c6ff1cbeac322de49d1b6df7c2032c61a830e3c17286de9462bf242fca2883585b93870a73853face6a6bf411198e9393920d483a7260bfb731fb5d25f1aa493335a9e71297e485b7aef312c21800deef121f1e76426a00665e5c4479674322d4f75edadd46debd5cd992f6ed090689d0585ff075ec9e99ad690c3395bc4b313370b38ef355acdadcd122975b12c85ea5db8c6deb4aab71808dcb408fe3d1e7690c43d37b4ce6cc0166fa7daa")
	testBytes := test[:64]
	expected, _ := hex.DecodeString("9c76476f4def4bb94541d57ebba1193381ffa7aa76ada664dd31c16024c43f59")

	resultBytes, err := g1Compress(testBytes)
	assert.NoError(t, err)
	assert.Equal(t, expected, resultBytes)
}

func Test_Altbn128_Decompress_G1(t *testing.T) {
	testBytes, _ := hex.DecodeString("289f03cf118d03ea0afaf7eee88b43be51de13626d6a2985e40e664cef94e497")
	expected, _ := hex.DecodeString("289f03cf118d03ea0afaf7eee88b43be51de13626d6a2985e40e664cef94e49708bfb9266f59fcbe9145d5f5fe53208581be9c57fd684db365c2ce7a58834508")

	resultBytes, err := g1Decompress(testBytes)
	assert.NoError(t, err)
	assert.Equal(t, expected, resultBytes)
}

func Test_Altbn128_Compress_G2(t *testing.T) {
	testBytes, _ := hex.DecodeString("25f83c8b6ab9de74e7da488ef02645c5a16a6652c3c71a15dc37fe3a5dcb7cb122acdedd6308e3bb230d226d16a105295f523a8a02bfc5e8bd2da135ac4c245d065bbad92e7c4e31bf3757f1fe7362a63fbfee50e7dc68da116e67d600d9bf6806d302580dc0661002994e7cd3a7f224e7ddc27802777486bf80f40e4ca3cfdb")
	expected, _ := hex.DecodeString("25f83c8b6ab9de74e7da488ef02645c5a16a6652c3c71a15dc37fe3a5dcb7cb122acdedd6308e3bb230d226d16a105295f523a8a02bfc5e8bd2da135ac4c245d")

	resultBytes, err := g2Compress(testBytes)
	assert.NoError(t, err)
	assert.Equal(t, expected, resultBytes)
}

func Test_Altbn128_Decompress_G2(t *testing.T) {
	testBytes, _ := hex.DecodeString("246e4678ca172cd2c062e8e52b188d52f57b4161ca776a31b72120ea16bbf6a5293332f0a799fd916f0fdd8bd92bd8c79f223551705dee901c8eef4c55b89927")
	expected, _ := hex.DecodeString("246e4678ca172cd2c062e8e52b188d52f57b4161ca776a31b72120ea16bbf6a5293332f0a799fd916f0fdd8bd92bd8c79f223551705dee901c8eef4c55b8992710f05ccdf51ea906f99e680eadecfa1ed52989d7e60c7f3b14ac97ff508e64a326c2dd798479e2c57282f5bdd7f0f5014af474393d32fc2eb6d5c30e6e4c22e0")

	resultBytes, err := g2Decompress(testBytes)
	assert.NoError(t, err)
	assert.Equal(t, expected, resultBytes)
}
