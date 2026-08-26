package rpcserver

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/filecoin-project/go-jsonrpc"
)

// GetRootedFeedStatusResp binds rooted publication to one AccountsDB lineage.
type GetRootedFeedStatusResp struct {
	Enabled             bool   `json:"enabled"`
	AccountsDBRootRunID string `json:"accountsDbRootRunId,omitempty"`
}

// SetRootedFeedIdentity publishes the rooted-event source identity once.
func (rpcServer *RpcServer) SetRootedFeedIdentity(rootRunID string) error {
	decoded, err := hex.DecodeString(rootRunID)
	if err != nil || len(decoded) != 4 && len(decoded) != 16 || hex.EncodeToString(decoded) != rootRunID {
		return errors.New("AccountsDB root run ID must be eight or 32 lowercase hexadecimal characters")
	}
	rpcServer.rootedFeedMu.Lock()
	defer rpcServer.rootedFeedMu.Unlock()
	if rpcServer.rootedFeedIdentitySet && rpcServer.accountsDBRootRunID != rootRunID {
		return errors.New("RPC rooted feed identity cannot change while the server is running")
	}
	rpcServer.rootedFeedIdentitySet = true
	rpcServer.rootedEventsEnabled = true
	rpcServer.accountsDBRootRunID = rootRunID
	return nil
}

// GetRootedFeedStatus returns configuration identity, not replay health.
func (rpcServer *RpcServer) GetRootedFeedStatus(
	_ context.Context,
	_ jsonrpc.RawParams,
) (GetRootedFeedStatusResp, error) {
	rpcServer.rootedFeedMu.RLock()
	defer rpcServer.rootedFeedMu.RUnlock()
	return GetRootedFeedStatusResp{
		Enabled:             rpcServer.rootedEventsEnabled,
		AccountsDBRootRunID: rpcServer.accountsDBRootRunID,
	}, nil
}
