package blockstream

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/stretchr/testify/assert"
)

func TestOvercastRecvStream(t *testing.T) {
	opts := BackgroundBlockDownloaderOpts{
		SourceType:       BackgroundBlockDownloaderSourceOvercast,
		OutDir:           "/tmp/overcast_blocks",
		OvercastEndpoint: "127.0.0.1:13370",
		RpcEndpoint:      "https://api.mainnet-beta.solana.com",
		StartSlot:        382240100,
	}

	downloader := NewBlockDownloader(opts)
	downloader.Start()
}

func TestDeser(t *testing.T) {
	blockFilename := "/tmp/blocks/383499316.json"
	file, err := os.Open(blockFilename)
	assert.NoError(t, err)

	// Create a decoder
	decoder := json.NewDecoder(file)

	out := &block.Block{}

	// Decode JSON into target
	err = decoder.Decode(out)
	assert.NoError(t, err)

	out.FixupTxVersions()
	fmt.Printf("FromOvercast? %t\n", out.FromOvercast)
}
