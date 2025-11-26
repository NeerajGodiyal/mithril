package blockstream

import (
	"testing"
)

func TestOvercastRecvStream(t *testing.T) {
	opts := BackgroundBlockDownloaderOpts{
		SourceType:       BackgroundBlockDownloaderSourceOvercast,
		OutDir:           "/tmp/overcast_blocks",
		OvercastEndpoint: "127.0.0.1:13370",
		RpcPoolFile:      "/home/ubuntu/rpcs.txt",
		StartSlot:        382240100,
	}

	downloader := NewBlockDownloader(opts)
	downloader.Start()
}
