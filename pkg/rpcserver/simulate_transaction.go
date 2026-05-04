package rpcserver

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/replay"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/filecoin-project/go-jsonrpc"
	"github.com/gagliardetto/solana-go"
	"github.com/mr-tron/base58"
)

type SimulateTransactionResp struct {
	Context SimulateTransactionRespContext `json:"context"`
	Value   SimulateTransactionRespValue   `json:"value"`
}

type SimulateTransactionRespContext struct {
	ApiVersion string `json:"apiVersion"`
	Slot       uint64 `json:"slot"`
}

type SimulateTransactionRespValue struct {
	Err                    interface{} `json:"err"`
	Logs                   []string    `json:"logs"`
	Accounts               interface{} `json:"accounts"`
	UnitsConsumed          *uint64     `json:"unitsConsumed,omitempty"`
	ReturnData             interface{} `json:"returnData"`
	InnerInstructions      interface{} `json:"innerInstructions"`
	ReplacementBlockhash   interface{} `json:"replacementBlockhash"`
	LoadedAccountsDataSize *uint32     `json:"loadedAccountsDataSize,omitempty"`
}

type simulateTransactionConfig struct {
	sigVerify              bool
	replaceRecentBlockhash bool
	encoding               string
	accounts               *simulateAccountsConfig
}

type simulateAccountsConfig struct {
	addresses []string
	encoding  string
}

func (rpcServer *RpcServer) SimulateTransaction(ctx context.Context, p jsonrpc.RawParams) (SimulateTransactionResp, error) {
	params, err := jsonrpc.DecodeParams[[]interface{}](p)
	if err != nil {
		return SimulateTransactionResp{}, fmt.Errorf("decoding params: %w", err)
	}

	if len(params) < 1 {
		return SimulateTransactionResp{}, fmt.Errorf("simulateTransaction requires a transaction string as first parameter")
	}

	txStr, ok := params[0].(string)
	if !ok {
		return SimulateTransactionResp{}, fmt.Errorf("simulateTransaction requires a transaction string as first parameter")
	}

	// Parse config
	conf := parseSimulateConfig(params)

	// Decode transaction
	var tx *solana.Transaction
	if conf.encoding == "base58" {
		tx, err = solana.TransactionFromBase58(txStr)
	} else {
		tx, err = solana.TransactionFromBase64(txStr)
	}
	if err != nil {
		return SimulateTransactionResp{}, fmt.Errorf("failed to decode transaction: %w", err)
	}

	// Validate sigVerify + replaceRecentBlockhash conflict
	if conf.sigVerify && conf.replaceRecentBlockhash {
		return SimulateTransactionResp{}, fmt.Errorf("sigVerify may not be used with replaceRecentBlockhash")
	}

	// Replace recent blockhash if requested
	var replacementBlockhash interface{}
	if conf.replaceRecentBlockhash {
		latestBlockhash := global.LatestBlockHash()
		tx.Message.RecentBlockhash = solana.Hash(latestBlockhash)
		blockHeight := global.BlockHeight()
		replacementBlockhash = map[string]interface{}{
			"blockhash":           base58.Encode(latestBlockhash[:]),
			"lastValidBlockHeight": blockHeight,
		}
	}

	// Verify signatures if requested
	if conf.sigVerify {
		err = tx.VerifySignatures()
		if err != nil {
			return SimulateTransactionResp{
				Context: SimulateTransactionRespContext{
					ApiVersion: "mithril 0.1",
					Slot:       global.Slot(),
				},
				Value: SimulateTransactionRespValue{
					Err:  "SignatureFailure",
					Logs: nil,
				},
			}, nil
		}
	}

	// Get SlotCtx
	slotCtx := rpcServer.getSlotCtx()
	if slotCtx == nil {
		return SimulateTransactionResp{}, fmt.Errorf("node is not ready for simulation")
	}

	// Resolve address-table lookups for versioned txs before the loader
	// runs. ALT misses surface as AddressLookupTableNotFound in-band.
	if err := replay.ResolveAddrTableLookupsForTx(ctx, rpcServer.acctsDb, slotCtx.Slot, tx); err != nil {
		return SimulateTransactionResp{
			Context: SimulateTransactionRespContext{
				ApiVersion: "mithril 0.1",
				Slot:       global.Slot(),
			},
			Value: SimulateTransactionRespValue{
				Err:                  "AddressLookupTableNotFound",
				ReplacementBlockhash: replacementBlockhash,
			},
		}, nil
	}

	// Execute transaction using the pure function
	output := replay.LoadAndExecuteTransaction(replay.LoadAndExecuteTransactionInput{
		SlotCtx:      slotCtx,
		Transaction:  tx,
		TxMeta:       nil,
		IsSimulation: true,
	})

	// Extract logs from ExecCtx if available
	var logs []string
	if output.ExecCtx != nil {
		if logRecorder, ok := output.ExecCtx.Log.(*sealevel.LogRecorder); ok && logRecorder != nil {
			logs = logRecorder.Logs
		}
	}

	resp := SimulateTransactionResp{
		Context: SimulateTransactionRespContext{
			ApiVersion: "mithril 0.1",
			Slot:       global.Slot(),
		},
		Value: SimulateTransactionRespValue{
			Logs:                 logs,
			ReplacementBlockhash: replacementBlockhash,
			InnerInstructions:    nil,
		},
	}

	// TransactionError.MarshalJSON renders the Agave wire format.
	if output.ProcessingResult.TransactionError != nil {
		resp.Value.Err = output.ProcessingResult.TransactionError
		return resp, nil
	}

	if output.ProcessingResult.ProcessedTransaction == nil {
		return resp, nil
	}

	processedTx := output.ProcessingResult.ProcessedTransaction

	if processedTx.TransactionType == replay.ProcessedTransactionTypeExecuted && processedTx.Executed != nil {
		executed := processedTx.Executed
		units := executed.ExecutionDetails.ExecutedUnits
		resp.Value.UnitsConsumed = &units
		dataSize := executed.LoadedTransaction.LoadedAccountsDataSize
		resp.Value.LoadedAccountsDataSize = &dataSize

		// Return data
		if executed.ExecutionDetails.ReturnData != nil {
			rd := executed.ExecutionDetails.ReturnData
			resp.Value.ReturnData = map[string]interface{}{
				"programId": base58.Encode(rd.ProgramId[:]),
				"data":      []string{base64.StdEncoding.EncodeToString(rd.Data), "base64"},
			}
		}

		// Execution status error
		if executed.ExecutionDetails.Status != nil {
			resp.Value.Err = executed.ExecutionDetails.Status.Error()
		}
	}

	// Handle requested accounts
	if conf.accounts != nil && output.ExecCtx != nil {
		accts := make([]interface{}, len(conf.accounts.addresses))
		for i, addrStr := range conf.accounts.addresses {
			pk, err := solana.PublicKeyFromBase58(addrStr)
			if err != nil {
				accts[i] = nil
				continue
			}

			// Look up account in the execution context's post-execution state
			found := false
			for idx, acct := range output.ExecCtx.TransactionContext.Accounts.Accounts {
				if acct.Key == pk {
					encodingType := GetAccountEncodingBase64
					if conf.accounts.encoding != "" {
						encodingType, _ = parseGetAcctDataEncodingType(conf.accounts.encoding)
					}
					acctConf := &GetAccountInfoConfig{EncodingType: &encodingType}
					encodedData, err := encodeAcctDataWithConfig(output.ExecCtx.TransactionContext.Accounts.Accounts[idx].Data, acctConf)
					if err != nil {
						accts[i] = nil
						continue
					}
					accts[i] = map[string]interface{}{
						"data":       encodedData,
						"executable": acct.Executable,
						"lamports":   acct.Lamports,
						"owner":      base58.Encode(acct.Owner[:]),
						"rentEpoch":  acct.RentEpoch,
						"space":      len(acct.Data),
					}
					found = true
					break
				}
			}
			if !found {
				accts[i] = nil
			}
		}
		resp.Value.Accounts = accts
	}

	return resp, nil
}

func parseSimulateConfig(params []interface{}) simulateTransactionConfig {
	conf := simulateTransactionConfig{
		encoding: "base64",
	}

	if len(params) < 2 {
		return conf
	}

	confMap, ok := params[1].(map[string]interface{})
	if !ok {
		return conf
	}

	if sigVerify, ok := confMap["sigVerify"].(bool); ok {
		conf.sigVerify = sigVerify
	}

	if replaceRecentBlockhash, ok := confMap["replaceRecentBlockhash"].(bool); ok {
		conf.replaceRecentBlockhash = replaceRecentBlockhash
	}

	if encoding, ok := confMap["encoding"].(string); ok {
		conf.encoding = encoding
	}

	if accountsObj, ok := confMap["accounts"].(map[string]interface{}); ok {
		acctConf := &simulateAccountsConfig{}
		if addresses, ok := accountsObj["addresses"].([]interface{}); ok {
			for _, addr := range addresses {
				if addrStr, ok := addr.(string); ok {
					acctConf.addresses = append(acctConf.addresses, addrStr)
				}
			}
		}
		if encoding, ok := accountsObj["encoding"].(string); ok {
			acctConf.encoding = encoding
		}
		conf.accounts = acctConf
	}

	return conf
}
