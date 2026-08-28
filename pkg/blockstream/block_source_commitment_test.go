package blockstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
)

func TestRPCCommitmentModeCoversTipBlockAndSkipProof(t *testing.T) {
	type request struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
		ID     json.RawMessage   `json:"id"`
	}

	var mu sync.Mutex
	seen := make(map[string][]string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var call request
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			t.Errorf("decode RPC request: %v", err)
			return
		}
		commitment := ""
		for _, param := range call.Params {
			var opts struct {
				Commitment string `json:"commitment"`
			}
			if json.Unmarshal(param, &opts) == nil && opts.Commitment != "" {
				commitment = opts.Commitment
			}
		}
		mu.Lock()
		seen[call.Method] = append(seen[call.Method], commitment)
		mu.Unlock()

		var result any
		switch call.Method {
		case "getSlot":
			result = uint64(123)
		case "getBlocksWithLimit":
			result = []uint64{101}
		case "getBlock":
			result = map[string]any{
				"blockhash":         "11111111111111111111111111111111",
				"previousBlockhash": "11111111111111111111111111111111",
				"parentSlot":        uint64(99),
				"transactions":      []any{},
				"rewards":           []any{},
			}
		case "getSlotLeaders":
			result = []string{"11111111111111111111111111111111"}
		default:
			t.Errorf("unexpected RPC method %q", call.Method)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": json.RawMessage(call.ID), "result": result,
		})
	}))
	defer server.Close()

	for _, test := range []struct {
		name          string
		finalizedOnly bool
		want          string
	}{
		{name: "default confirmed", want: "confirmed"},
		{name: "finalized only", finalizedOnly: true, want: "finalized"},
	} {
		t.Run(test.name, func(t *testing.T) {
			mu.Lock()
			clear(seen)
			mu.Unlock()
			blockDir := ""
			if test.finalizedOnly {
				blockDir = t.TempDir()
				if err := os.WriteFile(filepath.Join(blockDir, "100.json"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			bs := NewBlockSource(&BlockSourceOpts{
				RpcClient:     rpcclient.NewRpcClient(server.URL),
				SourceType:    BlockSourceRpc,
				FinalizedOnly: test.finalizedOnly,
				BlockDir:      blockDir,
			})
			if _, err := bs.getSchedulingTip(bs.rpcClients[0], time.Second); err != nil {
				t.Fatalf("get scheduling tip: %v", err)
			}
			if _, err := bs.getSlotsWithLimit(bs.rpcClients[0], 100, 1); err != nil {
				t.Fatalf("get slot listing: %v", err)
			}
			if _, err := bs.fetchBlockOnce(100, 0); err != nil {
				t.Fatalf("fetch block: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			for _, method := range []string{"getSlot", "getBlocksWithLimit", "getBlock"} {
				if got := seen[method]; len(got) != 1 || got[0] != test.want {
					t.Fatalf("%s commitments = %v, want [%s]", method, got, test.want)
				}
			}
		})
	}
}

func TestFileBlockRejectsMismatchedTransactionVersionsWithoutDeletingInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "42.json")
	data, err := json.Marshal(struct {
		Transactions []any   `json:"transactions"`
		Versions     []uint8 `json:"versions"`
	}{Versions: []uint8{0}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	bs := NewBlockSource(&BlockSourceOpts{BlockDir: dir})
	if _, err := bs.tryGetBlockFromFile(42); err == nil {
		t.Fatal("mismatched transaction versions were accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("invalid input was removed: %v", err)
	}
}

func TestSequentialFileSourceFinalizedOnlyBypassesFile(t *testing.T) {
	type request struct {
		Params []json.RawMessage `json:"params"`
		ID     json.RawMessage   `json:"id"`
	}
	commitment := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var call request
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			t.Errorf("decode RPC request: %v", err)
			return
		}
		for _, param := range call.Params {
			var opts struct {
				Commitment string `json:"commitment"`
			}
			if json.Unmarshal(param, &opts) == nil && opts.Commitment != "" {
				commitment = opts.Commitment
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": json.RawMessage(call.ID), "result": map[string]any{
				"blockhash":         "11111111111111111111111111111111",
				"previousBlockhash": "11111111111111111111111111111111",
				"parentSlot":        uint64(41),
				"transactions":      []any{},
				"rewards":           []any{},
			},
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "42.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bs := NewBlockSource(&BlockSourceOpts{
		RpcClient:     rpcclient.NewRpcClient(server.URL),
		SourceType:    BlockSourceFile,
		FinalizedOnly: true,
		BlockDir:      dir,
	})
	if _, err := bs.fetchAndParseBlockSequential(42); err != nil {
		t.Fatalf("fetch sequential block: %v", err)
	}
	if commitment != "finalized" {
		t.Fatalf("getBlock commitment = %q, want finalized", commitment)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("finalized-only replay consumed unproved block file: %v", err)
	}
}
