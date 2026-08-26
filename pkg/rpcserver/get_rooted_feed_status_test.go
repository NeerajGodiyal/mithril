package rpcserver

import (
	"context"
	"testing"
)

func TestRootedFeedStatusBindsOneAccountsDBLineage(t *testing.T) {
	server := &RpcServer{}
	status, err := server.GetRootedFeedStatus(context.Background(), nil)
	if err != nil || status.Enabled || status.AccountsDBRootRunID != "" {
		t.Fatalf("disabled status = %+v, %v", status, err)
	}
	if err := server.SetRootedFeedIdentity("not-hex!"); err == nil {
		t.Fatal("invalid root run ID was accepted")
	}
	if err := server.SetRootedFeedIdentity("0123abcd"); err != nil {
		t.Fatal(err)
	}
	if err := server.SetRootedFeedIdentity("0123abcd"); err != nil {
		t.Fatalf("idempotent identity: %v", err)
	}
	status, err = server.GetRootedFeedStatus(context.Background(), nil)
	if err != nil || !status.Enabled || status.AccountsDBRootRunID != "0123abcd" {
		t.Fatalf("enabled status = %+v, %v", status, err)
	}
	if err := server.SetRootedFeedIdentity("89abcdef"); err == nil {
		t.Fatal("changed root run ID was accepted")
	}
}

func TestRootedFeedStatusAcceptsCurrentLineageID(t *testing.T) {
	server := &RpcServer{}
	const rootRunID = "0123456789abcdef0123456789abcdef"
	if err := server.SetRootedFeedIdentity(rootRunID); err != nil {
		t.Fatal(err)
	}
	status, err := server.GetRootedFeedStatus(context.Background(), nil)
	if err != nil || status.AccountsDBRootRunID != rootRunID {
		t.Fatalf("status = %+v, %v", status, err)
	}
}
