package replay

import (
	"fmt"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/base58"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/stretchr/testify/assert"
)

func TestEahWorkaround(t *testing.T) {
	client := rpcclient.NewRpcClient("https://api.mainnet-beta.solana.com/")

	// test #1
	bankHash, err := fetchBankhashForSlot(client, 337646774)
	assert.NoError(t, err)

	bankHashStr := base58.Encode(bankHash)
	assert.Equal(t, "GzahP43kqpouTJrufyEehMhpjbu5BDvzPjLxbkzD647z", bankHashStr)
	fmt.Printf("bankhash: %s\n", base58.Encode(bankHash))

	// test #2
	bankHash, err = fetchBankhashForSlot(client, 337646505)
	assert.NoError(t, err)

	bankHashStr = base58.Encode(bankHash)
	assert.Equal(t, "GdCmxQrHfh2dgVZwjVX6SvPWLZUG5TDMXB339fVyMuhh", bankHashStr)
	fmt.Printf("bankhash: %s\n", base58.Encode(bankHash))

	// test #3
	bankHash, err = fetchBankhashForSlot(client, 337646220)
	assert.NoError(t, err)

	bankHashStr = base58.Encode(bankHash)
	assert.Equal(t, "MEtyFqQajLAfskQmbw28kTMSy2ASrg9KyShiM1TT2t6", bankHashStr)
	fmt.Printf("bankhash: %s\n", base58.Encode(bankHash))

	// test #4
	bankHash, err = fetchBankhashForSlot(client, 337645795)
	assert.NoError(t, err)

	bankHashStr = base58.Encode(bankHash)
	assert.Equal(t, "4ojc7a9ad4SVzWteAx2cvGH3JUbMvz2rXUv4ogr5DXYD", bankHashStr)
	fmt.Printf("bankhash: %s\n", base58.Encode(bankHash))

	// test #5
	bankHash, err = fetchBankhashForSlot(client, 337638540)
	assert.NoError(t, err)

	bankHashStr = base58.Encode(bankHash)
	assert.Equal(t, "8nPhRvJwtPia6NGigPbWr89FFpKD8mzWHovAbjxx6doM", bankHashStr)
	fmt.Printf("bankhash: %s\n", base58.Encode(bankHash))
}
