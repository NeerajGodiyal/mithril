package rpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/filecoin-project/go-jsonrpc"
	"github.com/gagliardetto/solana-go"
)

// testPubkey is a valid base58 account address used by every case here.
const testPubkey = "SysvarC1ock11111111111111111111111111111111"

// newAccountInfoServer builds a server whose account read is driven by read,
// so a test can distinguish a missing account from a failed one. The concrete
// AccountsDb cannot express a read failure without a corrupted store on disk.
func newAccountInfoServer(read func(uint64, solana.PublicKey) (*accounts.Account, error)) *RpcServer {
	server := &RpcServer{readAccount: read}
	server.SetSlotCtx(&sealevel.SlotCtx{Slot: 100})
	return server
}

func rawParams(t *testing.T, values ...any) jsonrpc.RawParams {
	t.Helper()
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("marshalling params: %v", err)
	}
	return jsonrpc.RawParams(encoded)
}

func foundAccount() *accounts.Account {
	return &accounts.Account{
		Lamports:   4200,
		Data:       []byte("hello"),
		Owner:      [32]byte{1},
		Executable: false,
		RentEpoch:  7,
	}
}

// TestGetAccountInfoSeparatesMissingFromFailed is the central contract. Solana
// says a missing account is context plus a null value; a read failure is an
// error. Collapsing the two lets a caller act on "no balance" during a disk
// fault, which is the worst possible outcome for anything automated.
func TestGetAccountInfoSeparatesMissingFromFailed(t *testing.T) {
	t.Run("missing account is a null value, not an error", func(t *testing.T) {
		server := newAccountInfoServer(func(uint64, solana.PublicKey) (*accounts.Account, error) {
			return nil, accountsdb.ErrNoAccount
		})
		resp, err := server.GetAccountInfo(context.Background(), rawParams(t, testPubkey))
		if err != nil {
			t.Fatalf("a missing account was reported as an error: %v", err)
		}
		if resp.Value != nil {
			t.Errorf("missing account returned a value: %+v", resp.Value)
		}
		// The context must still be well formed — a caller reads the slot to
		// decide whether the answer is fresh enough to act on.
		if resp.Context.ApiVersion == "" {
			t.Error("missing-account response carries no apiVersion")
		}
	})

	t.Run("read failure is an error, not an empty account", func(t *testing.T) {
		readErr := errors.New("accountsdb: read failed after retry: input/output error")
		server := newAccountInfoServer(func(uint64, solana.PublicKey) (*accounts.Account, error) {
			return nil, readErr
		})
		resp, err := server.GetAccountInfo(context.Background(), rawParams(t, testPubkey))
		if err == nil {
			t.Fatalf("a read failure was reported as success: %+v", resp)
		}
		if resp.Value != nil {
			t.Errorf("a failed read returned a value: %+v", resp.Value)
		}
	})

	t.Run("a found account is returned", func(t *testing.T) {
		server := newAccountInfoServer(func(uint64, solana.PublicKey) (*accounts.Account, error) {
			return foundAccount(), nil
		})
		resp, err := server.GetAccountInfo(context.Background(), rawParams(t, testPubkey))
		if err != nil {
			t.Fatalf("a present account failed: %v", err)
		}
		if resp.Value == nil {
			t.Fatal("a present account returned a null value")
		}
		if resp.Value.Lamports != 4200 || resp.Value.Space != 5 {
			t.Errorf("account rendered as %+v", resp.Value)
		}
	})
}

// TestGetAccountInfoContextSlotIsTheReadSlot pins that the advertised slot
// describes the read that happened. Reporting an unrelated global slot invites
// a caller to believe the data is newer than it is.
func TestGetAccountInfoContextSlotIsTheReadSlot(t *testing.T) {
	var observed uint64
	server := newAccountInfoServer(func(slot uint64, _ solana.PublicKey) (*accounts.Account, error) {
		observed = slot
		return foundAccount(), nil
	})

	resp, err := server.GetAccountInfo(context.Background(), rawParams(t, testPubkey))
	if err != nil {
		t.Fatalf("GetAccountInfo: %v", err)
	}
	if resp.Context.Slot != observed {
		t.Errorf("advertised slot %d but read at slot %d", resp.Context.Slot, observed)
	}

	// The same slot must be reported when the account is absent, otherwise the
	// two paths describe different points in time.
	missing := newAccountInfoServer(func(slot uint64, _ solana.PublicKey) (*accounts.Account, error) {
		observed = slot
		return nil, accountsdb.ErrNoAccount
	})
	missingResp, err := missing.GetAccountInfo(context.Background(), rawParams(t, testPubkey))
	if err != nil {
		t.Fatalf("missing account: %v", err)
	}
	if missingResp.Context.Slot != observed {
		t.Errorf("missing path advertised slot %d but read at %d", missingResp.Context.Slot, observed)
	}
}

// TestGetAccountInfoRejectsUnprovableCommitment covers commitment handling.
// The node replays and can describe its own processed state; it cannot prove
// cluster confirmation or finality. Accepting those words and answering with
// processed data would be a silent lie.
func TestGetAccountInfoRejectsUnprovableCommitment(t *testing.T) {
	server := newAccountInfoServer(func(uint64, solana.PublicKey) (*accounts.Account, error) {
		return foundAccount(), nil
	})

	for _, commitment := range []string{"confirmed", "finalized"} {
		params := rawParams(t, testPubkey, map[string]any{"commitment": commitment})
		if _, err := server.GetAccountInfo(context.Background(), params); err == nil {
			t.Errorf("commitment %q was accepted", commitment)
		} else if !strings.Contains(err.Error(), commitment) {
			t.Errorf("error for %q does not name the commitment: %v", commitment, err)
		}
	}

	for _, commitment := range []string{"", "processed"} {
		params := rawParams(t, testPubkey, map[string]any{"commitment": commitment})
		if _, err := server.GetAccountInfo(context.Background(), params); err != nil {
			t.Errorf("supported commitment %q was rejected: %v", commitment, err)
		}
	}
}

// TestGetAccountInfoEnforcesMinContextSlot covers minContextSlot, which was
// parsed into the config struct but never read. A caller uses it to refuse
// data older than a slot it already saw.
func TestGetAccountInfoEnforcesMinContextSlot(t *testing.T) {
	server := newAccountInfoServer(func(uint64, solana.PublicKey) (*accounts.Account, error) {
		return foundAccount(), nil
	})

	t.Run("below the node's slot is served", func(t *testing.T) {
		params := rawParams(t, testPubkey, map[string]any{"minContextSlot": float64(99)})
		if _, err := server.GetAccountInfo(context.Background(), params); err != nil {
			t.Errorf("minContextSlot below the current slot was rejected: %v", err)
		}
	})

	t.Run("equal to the node's slot is served", func(t *testing.T) {
		params := rawParams(t, testPubkey, map[string]any{"minContextSlot": float64(100)})
		if _, err := server.GetAccountInfo(context.Background(), params); err != nil {
			t.Errorf("minContextSlot equal to the current slot was rejected: %v", err)
		}
	})

	t.Run("above the node's slot is refused", func(t *testing.T) {
		params := rawParams(t, testPubkey, map[string]any{"minContextSlot": float64(101)})
		if _, err := server.GetAccountInfo(context.Background(), params); err == nil {
			t.Error("minContextSlot ahead of the node was served anyway")
		}
	})
}

// TestGetAccountInfoRejectsMalformedInput keeps the existing parameter
// validation honest while the surrounding behaviour changes.
func TestGetAccountInfoRejectsMalformedInput(t *testing.T) {
	server := newAccountInfoServer(func(uint64, solana.PublicKey) (*accounts.Account, error) {
		return foundAccount(), nil
	})

	cases := map[string]jsonrpc.RawParams{
		"no params":        rawParams(t),
		"non-string key":   rawParams(t, 42),
		"invalid base58":   rawParams(t, "not-a-valid-key!!!"),
		"bad config type":  rawParams(t, testPubkey, "not-an-object"),
		"unknown encoding": rawParams(t, testPubkey, map[string]any{"encoding": "base99"}),
		"too many params":  rawParams(t, testPubkey, map[string]any{}, "extra"),
		"fractional slot":  rawParams(t, testPubkey, map[string]any{"minContextSlot": 1.5}),
		"slice missing":    rawParams(t, testPubkey, map[string]any{"dataSlice": map[string]any{"offset": 0.0}}),
		"slice fractional": rawParams(t, testPubkey, map[string]any{"dataSlice": map[string]any{"offset": 0.0, "length": 1.5}}),
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := server.GetAccountInfo(context.Background(), params); err == nil {
				t.Error("malformed input was accepted")
			}
		})
	}
}
