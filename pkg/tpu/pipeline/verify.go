package pipeline

import "github.com/Overclock-Validator/mithril/pkg/tpu/sigverify"

func verifyPacket(data []byte) bool {
	return sigverify.VerifyPacket(data)
}
