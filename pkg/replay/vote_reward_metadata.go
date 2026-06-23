package replay

import "github.com/gagliardetto/solana-go"

// AlpenglowFeatureGatePubkey is the on-chain Alpenglow feature gate account.
// We derivee internal PDAs (vote_reward_account, alpenclock, etc.) using this
// pubkey as the program_id argument to find_program_address.
const AlpenglowFeatureGatePubkey = "a1penGLz8Vm2QHYB3JPefBiU4BY3Z6JkW2k3Scw5GWP"

var (
	alpenglowFeatureGatePubkey = solana.MustPublicKeyFromBase58(AlpenglowFeatureGatePubkey)
	voteRewardAccountPubkey, _, _ = solana.FindProgramAddress([][]byte{[]byte("vote_reward_account")}, alpenglowFeatureGatePubkey)
)

// VoteRewardAccountAddr returns the vote-reward metadata PDA (Agave epoch inflation state).
func VoteRewardAccountAddr() solana.PublicKey {
	return voteRewardAccountPubkey
}
