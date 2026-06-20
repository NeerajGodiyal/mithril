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

func TestAddConfiguredSnapshotRPCNodes(t *testing.T) {
	nodes := []snaprpc.RPCNode{
		{Address: "http://public.example.com:8899", Version: "cluster"},
	}

	got, added := addConfiguredSnapshotRPCNodes(nodes, "https://rpc.example.com", []string{
		"https://rpc.example.com",
		"http://public.example.com:8899",
	})
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[1].Address != "https://rpc.example.com" {
		t.Fatalf("configured address = %q", got[1].Address)
	}
	if got[1].Version != "999.0.0-configured" {
		t.Fatalf("configured version = %q", got[1].Version)
	}
	if !got[1].IsStatic {
		t.Fatalf("configured RPC was not marked static")
	}
}

func TestFilterByIncrementalBaseMatchAllowsGenesisFullWithBaseZeroIncremental(t *testing.T) {
	results := []snaprpc.NodeResult{
		{
			RPC:      "http://young-testnet.example.com:8899",
			FullURL:  "http://young-testnet.example.com:8899/snapshot-0-hash.tar.zst",
			FullSlot: 0,
			HasInc:   true,
			IncBase:  0,
			IncSlot:  21102,
			IncURL:   "http://young-testnet.example.com:8899/incremental-snapshot-0-21102-hash.tar.zst",
		},
	}

	filtered, stats := filterByIncrementalBaseMatch(results)
	if len(filtered) != 1 {
		t.Fatalf("len(filtered) = %d, want 1", len(filtered))
	}
	if stats.totalWithFull != 1 || stats.totalWithFullSlotZero != 1 {
		t.Fatalf("full stats = %d/%d, want 1/1", stats.totalWithFull, stats.totalWithFullSlotZero)
	}
	if stats.matchingFullSlots != 1 {
		t.Fatalf("matchingFullSlots = %d, want 1", stats.matchingFullSlots)
	}
}

func TestNormalizeGenesisFullSnapshotsForRankingUsesSyntheticSlot(t *testing.T) {
	results := []snaprpc.NodeResult{
		{
			RPC:      "http://young-testnet.example.com:8899",
			FullURL:  "http://young-testnet.example.com:8899/snapshot-0-hash.tar.zst",
			FullSlot: 0,
			HasInc:   true,
			IncBase:  0,
		},
	}

	normalized := normalizeGenesisFullSnapshotsForRanking(results)
	if results[0].FullSlot != 0 {
		t.Fatalf("original FullSlot mutated to %d", results[0].FullSlot)
	}
	if normalized[0].FullSlot != 1 {
		t.Fatalf("normalized FullSlot = %d, want synthetic slot 1", normalized[0].FullSlot)
	}
}

func TestRelaxedSnapshotProbeConfigUsesTinyStage1Sample(t *testing.T) {
	cfg := DefaultSnapshotConfig().toInternalConfig("")
	relaxed := relaxedSnapshotProbeConfig(cfg)

	if relaxed.Stage1WarmKiB != 0 {
		t.Fatalf("Stage1WarmKiB = %d, want 0", relaxed.Stage1WarmKiB)
	}
	if relaxed.Stage1WindowKiB != 64 {
		t.Fatalf("Stage1WindowKiB = %d, want 64", relaxed.Stage1WindowKiB)
	}
	if relaxed.Stage1Windows != 1 {
		t.Fatalf("Stage1Windows = %d, want 1", relaxed.Stage1Windows)
	}
}
