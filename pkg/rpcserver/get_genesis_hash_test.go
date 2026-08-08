package rpcserver

import (
	"context"
	"testing"

	"github.com/filecoin-project/go-jsonrpc"
)

const devnetGenesisHash = "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG"

func TestGetGenesisHashRequiresConfiguredIdentity(t *testing.T) {
	server := &RpcServer{}
	params := rawParams(t)
	if _, err := server.GetGenesisHash(context.Background(), params); err == nil {
		t.Fatal("unconfigured genesis hash was returned")
	}
	if err := server.SetGenesisHash(devnetGenesisHash); err != nil {
		t.Fatal(err)
	}
	got, err := server.GetGenesisHash(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if got != devnetGenesisHash {
		t.Fatalf("genesis hash = %q, want %q", got, devnetGenesisHash)
	}
}

func TestGetGenesisHashRejectsInvalidStateAndParams(t *testing.T) {
	server := &RpcServer{}
	for _, value := range []string{"", "not-base58", "11111111111111111111111111111111"} {
		if err := server.SetGenesisHash(value); err == nil {
			t.Fatalf("invalid genesis hash %q was accepted", value)
		}
	}
	if err := server.SetGenesisHash(devnetGenesisHash); err != nil {
		t.Fatal(err)
	}
	if _, err := server.GetGenesisHash(context.Background(), rawParams(t, true)); err == nil {
		t.Fatal("unexpected parameters were accepted")
	}
	if err := server.SetGenesisHash("5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"); err == nil {
		t.Fatal("configured genesis hash was changed")
	}
	if _, err := server.GetGenesisHash(context.Background(), jsonrpc.RawParams(`{`)); err == nil {
		t.Fatal("malformed parameters were accepted")
	}
}
