package rewards

import (
	"testing"

	"github.com/gagliardetto/solana-go"
)

func TestInterpreter_Sip(t *testing.T) {
	blockhash := solana.MustPublicKeyFromBase58("GwnQEJWfgbmNf9uehS3n4w4pYghhQCxH26EAFntHoTsT")
	pk := solana.MustPublicKeyFromBase58("8NdhUaU9ZgSuXQ2m9E6r8sDV5qTHJdtq1KC9Ftr8h2F")

	CalculateRewardPartitionForPubkey(pk, blockhash, 319)
}
