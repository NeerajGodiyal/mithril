package blocksub

import (
	"testing"
)

func TestBlockSubscribe(t *testing.T) {
	opts := BlockSubscriberOpts{Endpoint: "XYXY", OutDir: "XYXY", EndpointType: BlockEndpointTypeLaserStream, ApiKey: "XYXY"}
	blockSubscriber := NewBlockSubscriber(opts)
	go blockSubscriber.Start()

	for {
	}
}
