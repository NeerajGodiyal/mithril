package snapshotdl

import (
	"testing"

	snaprpc "github.com/Overclock-Validator/solana-snapshot-finder-go/pkg/rpc"
)

func TestSnapshotNodeBlacklistMatchesCommonEndpointForms(t *testing.T) {
	blacklist := newSnapshotNodeBlacklist([]string{
		"http://203.0.113.10:8899/",
		"198.51.100.24:8899",
		"bad-snapshot-node.example.com",
	})

	tests := []struct {
		name     string
		endpoint string
		want     bool
	}{
		{
			name:     "full rpc url",
			endpoint: "http://203.0.113.10:8899",
			want:     true,
		},
		{
			name:     "snapshot url from same source",
			endpoint: "https://203.0.113.10:8899/snapshot-123-abc.tar.zst",
			want:     true,
		},
		{
			name:     "host port",
			endpoint: "http://198.51.100.24:8899",
			want:     true,
		},
		{
			name:     "hostname",
			endpoint: "http://bad-snapshot-node.example.com:8899",
			want:     true,
		},
		{
			name:     "allowed node",
			endpoint: "http://good-snapshot-node.example.com:8899",
			want:     false,
		},
		{
			name:     "empty endpoint",
			endpoint: "",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := blacklist.contains(tt.endpoint); got != tt.want {
				t.Fatalf("contains(%q) = %v, want %v", tt.endpoint, got, tt.want)
			}
		})
	}
}

func TestFilterSnapshotRPCNodes(t *testing.T) {
	blacklist := newSnapshotNodeBlacklist([]string{"203.0.113.10", "bad.example.com"})
	nodes := []snaprpc.RPCNode{
		{Address: "http://203.0.113.10:8899"},
		{Address: "http://good.example.com:8899"},
		{Address: "http://bad.example.com:8899"},
	}

	filtered, skipped := filterSnapshotRPCNodes(nodes, blacklist)
	if skipped != 2 {
		t.Fatalf("skipped = %d, want 2", skipped)
	}
	if len(filtered) != 1 {
		t.Fatalf("len(filtered) = %d, want 1", len(filtered))
	}
	if filtered[0].Address != "http://good.example.com:8899" {
		t.Fatalf("remaining node = %q, want good.example.com", filtered[0].Address)
	}
}
