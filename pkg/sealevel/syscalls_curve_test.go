package sealevel

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
)

var input1 = "2bd3e6d0f3b142924f5ca7b49ce5b9d54c4703d7ae5648e61d02268b1a0a9fb721611ce0a6af85915e2f1d70300909ce2e49dfad4a4619c8390cae66cefdb20400000000000000000000000000000000000000000000000011138ce750fa15c2"
var result1 = "070a8d6a982153cae4be29d434e8faef8a47b274a053f5a4ee2a6c9c13c31e5c031b8ce914eba3a9ffb989f9cdd5b0f01943074bf4f0f315690ec3cec6981afc"

var input2 = "070a8d6a982153cae4be29d434e8faef8a47b274a053f5a4ee2a6c9c13c31e5c031b8ce914eba3a9ffb989f9cdd5b0f01943074bf4f0f315690ec3cec6981afc30644e72e131a029b85045b68181585d97816a916871ca8d3c208c16d87cfd46"
var result2 = "025a6f4181d2b4ea8b724290ffb40156eb0adb514c688556eb79cdea0752c2bb2eff3f31dea215f1eb86023a133a996eb6300b44da664d64251d05381bb8a02e"

var input3 = "025a6f4181d2b4ea8b724290ffb40156eb0adb514c688556eb79cdea0752c2bb2eff3f31dea215f1eb86023a133a996eb6300b44da664d64251d05381bb8a02e183227397098d014dc2822db40c0ac2ecbc0b548b438e5469e10460b6c3e7ea3"
var result3 = "14789d0d4a730b354403b5fac948113739e276c23e0258d8596ee72f9cd9d3230af18a63153e0ec25ff9f2951dd3fa90ed0197bfef6e2a1a62b5095b9d2b4a27"

var input4 = "1a87b0584ce92f4593d161480614f2989035225609f08058ccfa3d0f940febe31a2f3c951f6dadcc7ee9007dff81504b0fcd6d7cf59996efdc33d92bf7f9f8f6ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
var result4 = "2cde5879ba6f13c0b5aa4ef627f159a3347df9722efce88a9afbb20b763b4c411aa7e43076f6aee272755a7f9b84832e71559ba0d2e0b17d5f9f01755e5b0d11"

var input5 = "1a87b0584ce92f4593d161480614f2989035225609f08058ccfa3d0f940febe31a2f3c951f6dadcc7ee9007dff81504b0fcd6d7cf59996efdc33d92bf7f9f8f630644e72e131a029b85045b68181585d2833e84879b9709143e1f593f0000000"
var result5 = "1a87b0584ce92f4593d161480614f2989035225609f08058ccfa3d0f940febe3163511ddc1c3f25d396745388200081287b3fd1472d8339d5fecb2eae0830451"

var input6 = "1a87b0584ce92f4593d161480614f2989035225609f08058ccfa3d0f940febe31a2f3c951f6dadcc7ee9007dff81504b0fcd6d7cf59996efdc33d92bf7f9f8f60000000000000000000000000000000100000000000000000000000000000000"
var result6 = "1051acb0700ec6d42a88215852d582efbaef31529b6fcbc3277b5c1b300f5cf0135b2394bb45ab04b8bd7611bd2dfe1de6a4e6e2ccea1ea1955f577cd66af85b"

var input7 = "1a87b0584ce92f4593d161480614f2989035225609f08058ccfa3d0f940febe31a2f3c951f6dadcc7ee9007dff81504b0fcd6d7cf59996efdc33d92bf7f9f8f60000000000000000000000000000000000000000000000000000000000000009"
var result7 = "1dbad7d39dbc56379f78fac1bca147dc8e66de1b9d183c7b167351bfe0aeab742cd757d51289cd8dbd0acf9e673ad67d0f0a89f912af47ed1be53664f5692575"

var input8 = "1a87b0584ce92f4593d161480614f2989035225609f08058ccfa3d0f940febe31a2f3c951f6dadcc7ee9007dff81504b0fcd6d7cf59996efdc33d92bf7f9f8f60000000000000000000000000000000000000000000000000000000000000001"
var result8 = "1a87b0584ce92f4593d161480614f2989035225609f08058ccfa3d0f940febe31a2f3c951f6dadcc7ee9007dff81504b0fcd6d7cf59996efdc33d92bf7f9f8f6"

var input9 = "17c139df0efee0f766bc0204762b774362e4ded88953a39ce849a8a7fa163fa901e0559bacb160664764a357af8a9fe70baa9258e0b959273ffc5718c6d4cc7cffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
var result9 = "29e587aadd7c06722aabba753017c093f70ba7eb1f1c0104ec0564e7e3e21f6022b1143f6a41008e7755c71c3d00b6b915d386de21783ef590486d8afa8453b1"

var input10 = "17c139df0efee0f766bc0204762b774362e4ded88953a39ce849a8a7fa163fa901e0559bacb160664764a357af8a9fe70baa9258e0b959273ffc5718c6d4cc7c30644e72e131a029b85045b68181585d2833e84879b9709143e1f593f0000000"
var result10 = "17c139df0efee0f766bc0204762b774362e4ded88953a39ce849a8a7fa163fa92e83f8d734803fc370eba25ed1f6b8768bd6d83887b87165fc2434fe11a830cb"

var input11 = "17c139df0efee0f766bc0204762b774362e4ded88953a39ce849a8a7fa163fa901e0559bacb160664764a357af8a9fe70baa9258e0b959273ffc5718c6d4cc7c0000000000000000000000000000000100000000000000000000000000000000"
var result11 = "221a3577763877920d0d14a91cd59b9479f83b87a653bb41f82a3f6f120cea7c2752c7f64cdd7f0e494bff7b60419f242210f2026ed2ec70f89f78a4c56a1f15"

var input12 = "17c139df0efee0f766bc0204762b774362e4ded88953a39ce849a8a7fa163fa901e0559bacb160664764a357af8a9fe70baa9258e0b959273ffc5718c6d4cc7c0000000000000000000000000000000000000000000000000000000000000009"
var result12 = "228e687a379ba154554040f8821f4e41ee2be287c201aa9c3bc02c9dd12f1e691e0fd6ee672d04cfd924ed8fdc7ba5f2d06c53c1edc30f65f2af5a5b97f0a76a"

var input13 = "17c139df0efee0f766bc0204762b774362e4ded88953a39ce849a8a7fa163fa901e0559bacb160664764a357af8a9fe70baa9258e0b959273ffc5718c6d4cc7c0000000000000000000000000000000000000000000000000000000000000001"
var result13 = "17c139df0efee0f766bc0204762b774362e4ded88953a39ce849a8a7fa163fa901e0559bacb160664764a357af8a9fe70baa9258e0b959273ffc5718c6d4cc7c"

var input14 = "039730ea8dff1254c0fee9c0ea777d29a9c710b7e616683f194f18c43b43b869073a5ffcc6fc7a28c30723d6e58ce577356982d65b833a5a5c15bf9024b43d98ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
var result14 = "00a1a234d08efaa2616607e31eca1980128b00b415c845ff25bba3afcb81dc00242077290ed33906aeb8e42fd98c41bcb9057ba03421af3f2d08cfc441186024"

var input15 = "039730ea8dff1254c0fee9c0ea777d29a9c710b7e616683f194f18c43b43b869073a5ffcc6fc7a28c30723d6e58ce577356982d65b833a5a5c15bf9024b43d9830644e72e131a029b85045b68181585d2833e84879b9709143e1f593f0000000"
var result15 = "039730ea8dff1254c0fee9c0ea777d29a9c710b7e616683f194f18c43b43b8692929ee761a352600f54921df9bf472e66217e7bb0cee9032e00acc86b3c8bfaf"

var input16 = "039730ea8dff1254c0fee9c0ea777d29a9c710b7e616683f194f18c43b43b869073a5ffcc6fc7a28c30723d6e58ce577356982d65b833a5a5c15bf9024b43d980000000000000000000000000000000100000000000000000000000000000000"
var result16 = "1071b63011e8c222c5a771dfa03c2e11aac9666dd097f2c620852c3951a4376a2f46fe2f73e1cf310a168d56baa5575a8319389d7bfa6b29ee2d908305791434"

var input17 = "039730ea8dff1254c0fee9c0ea777d29a9c710b7e616683f194f18c43b43b869073a5ffcc6fc7a28c30723d6e58ce577356982d65b833a5a5c15bf9024b43d980000000000000000000000000000000000000000000000000000000000000009"
var result17 = "19f75b9dd68c080a688774a6213f131e3052bd353a304a189d7a2ee367e3c2582612f545fb9fc89fde80fd81c68fc7dcb27fea5fc124eeda69433cf5c46d2d7f"

var input18 = "039730ea8dff1254c0fee9c0ea777d29a9c710b7e616683f194f18c43b43b869073a5ffcc6fc7a28c30723d6e58ce577356982d65b833a5a5c15bf9024b43d980000000000000000000000000000000000000000000000000000000000000001"
var result18 = "039730ea8dff1254c0fee9c0ea777d29a9c710b7e616683f194f18c43b43b869073a5ffcc6fc7a28c30723d6e58ce577356982d65b833a5a5c15bf9024b43d98"

var input19 = "039730ea8dff1254c0fee9c0ea777d29a9c710b7e616683f194f18c43b43b869073a5ffcc6fc7a28c30723d6e58ce577356982d65b833a5a5c15bf9024b43d98"
var result19 = "00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"

var input20 = "039730ea8dff1254c0fee9c0ea777d29a9c710b7e616683f194f18c43b43b869073a5ffcc6fc7a28c30723d6e58ce577356982d65b833a5a5c15bf9024b43d9800000000000000000000000000000001"
var result20 = "1071b63011e8c222c5a771dfa03c2e11aac9666dd097f2c620852c3951a4376a2f46fe2f73e1cf310a168d56baa5575a8319389d7bfa6b29ee2d908305791434"

func TestConformance_AltBn128_Mul1(t *testing.T) {
	in, err := hex.DecodeString(input1)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result1)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul2(t *testing.T) {
	in, err := hex.DecodeString(input2)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result2)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul3(t *testing.T) {
	in, err := hex.DecodeString(input3)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result3)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul4(t *testing.T) {
	in, err := hex.DecodeString(input4)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result4)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul5(t *testing.T) {
	in, err := hex.DecodeString(input5)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result5)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul6(t *testing.T) {
	in, err := hex.DecodeString(input6)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result6)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul7(t *testing.T) {
	in, err := hex.DecodeString(input7)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result7)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul8(t *testing.T) {
	in, err := hex.DecodeString(input8)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result8)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul9(t *testing.T) {
	in, err := hex.DecodeString(input9)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result9)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul10(t *testing.T) {
	in, err := hex.DecodeString(input10)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result10)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul11(t *testing.T) {
	in, err := hex.DecodeString(input11)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result11)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul12(t *testing.T) {
	in, err := hex.DecodeString(input12)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result12)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul13(t *testing.T) {
	in, err := hex.DecodeString(input13)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result13)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul14(t *testing.T) {
	in, err := hex.DecodeString(input14)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result14)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul15(t *testing.T) {
	in, err := hex.DecodeString(input15)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result15)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul16(t *testing.T) {
	in, err := hex.DecodeString(input16)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result16)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul17(t *testing.T) {
	in, err := hex.DecodeString(input17)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result17)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul18(t *testing.T) {
	in, err := hex.DecodeString(input18)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result18)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul19(t *testing.T) {
	in, err := hex.DecodeString(input19)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result19)

	assert.Equal(t, out, correct)
}

func TestConformance_AltBn128_Mul20(t *testing.T) {
	in, err := hex.DecodeString(input20)
	assert.NoError(t, err)

	out, err := altbn128Multiplication(in, 128)
	assert.NoError(t, err)

	correct, err := hex.DecodeString(result20)

	assert.Equal(t, out, correct)
}
