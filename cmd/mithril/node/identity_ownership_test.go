package node

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	solana "github.com/gagliardetto/solana-go"
)

func TestSameUDPAddrString(t *testing.T) {
	tests := []struct {
		name     string
		observed string
		expected *net.UDPAddr
		want     bool
	}{
		{name: "ipv4 match", observed: "138.201.82.134:8020", expected: &net.UDPAddr{IP: net.ParseIP("138.201.82.134"), Port: 8020}, want: true},
		{name: "different port", observed: "138.201.82.134:9000", expected: &net.UDPAddr{IP: net.ParseIP("138.201.82.134"), Port: 8020}},
		{name: "different host", observed: "203.0.113.1:8020", expected: &net.UDPAddr{IP: net.ParseIP("138.201.82.134"), Port: 8020}},
		{name: "invalid", observed: "not-an-address", expected: &net.UDPAddr{IP: net.ParseIP("138.201.82.134"), Port: 8020}},
		{name: "nil expected", observed: "138.201.82.134:8020", expected: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameUDPAddrString(tc.observed, tc.expected); got != tc.want {
				t.Fatalf("sameUDPAddrString(%q, %v) = %v, want %v", tc.observed, tc.expected, got, tc.want)
			}
		})
	}
}

func TestQueryValidatorIdentityGossip(t *testing.T) {
	identity := solana.NewWallet().PublicKey()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","result":[{"pubkey":"%s","gossip":"138.201.82.134:9000","shredVersion":10638}],"id":1}`, identity.String())
	}))
	defer server.Close()

	got, found, err := queryValidatorIdentityGossip(t.Context(), []string{server.URL}, identity)
	if err != nil {
		t.Fatalf("queryValidatorIdentityGossip returned error: %v", err)
	}
	if !found || got != "138.201.82.134:9000" {
		t.Fatalf("queryValidatorIdentityGossip = %q, %v; want advertised contact", got, found)
	}

	missing, found, err := queryValidatorIdentityGossip(t.Context(), []string{server.URL}, solana.NewWallet().PublicKey())
	if err != nil {
		t.Fatalf("queryValidatorIdentityGossip missing returned error: %v", err)
	}
	if found || missing != "" {
		t.Fatalf("missing identity = %q, %v; want absent", missing, found)
	}
}
