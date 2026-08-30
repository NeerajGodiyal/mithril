package rpcserver

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/replay"
)

type publicProvenanceFixture struct {
	GenesisHash        string          `json:"genesis_hash"`
	VerificationStatus json.RawMessage `json:"verification_status"`
}

func TestWalletlessProvenanceGoldenContract(t *testing.T) {
	server := &RpcServer{
		verificationSnapshot: snapshotReturning(replay.VerificationComplete, 91, 91),
	}
	if err := server.SetGenesisHash(devnetGenesisHash); err != nil {
		t.Fatal(err)
	}
	server.rpcService = newRPCService(server)

	genesisWire := contractRPCResult(t, server, "getGenesisHash")
	var genesis string
	if err := json.Unmarshal(genesisWire, &genesis); err != nil {
		t.Fatal(err)
	}
	statusWire := contractRPCResult(t, server, "getVerificationStatus")
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(statusWire, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"state", "required", "verifiedSlot", "eligibleSlot", "healthy", "evidenceServed"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("getVerificationStatus result is missing exact wire key %q: %s", key, statusWire)
		}
		delete(fields, key)
	}
	if len(fields) != 0 {
		t.Fatalf("getVerificationStatus result has unexpected wire keys: %v", fields)
	}
	var status GetVerificationStatusResp
	if err := json.Unmarshal(statusWire, &status); err != nil {
		t.Fatal(err)
	}
	if status.State != string(replay.VerificationComplete) || !status.Required ||
		status.VerifiedSlot != 91 || status.EligibleSlot != 91 ||
		!status.Healthy || !status.EvidenceServed || status.Reason != "" {
		t.Fatalf("getVerificationStatus semantics = %+v", status)
	}

	got, err := json.Marshal(publicProvenanceFixture{
		GenesisHash: genesis, VerificationStatus: statusWire,
	})
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile("testdata/walletless-provenance-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("walletless provenance fixture drifted\ngot:  %s\nwant: %s", got, want)
	}
}

func contractRPCResult(t *testing.T, server *RpcServer, method string) json.RawMessage {
	t.Helper()
	payload, err := server.executeRPCRequestWithID(context.Background(), rpcMethodProbe{
		JSONRPC: "2.0", Method: method, ID: json.RawMessage(`1`), Params: json.RawMessage(`[]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatal(err)
	}
	if response.JSONRPC != "2.0" || string(response.ID) != "1" || len(response.Result) == 0 {
		t.Fatalf("%s response = %s", method, payload)
	}
	return response.Result
}
