package rpcserver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/DataDog/zstd"
	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/filecoin-project/go-jsonrpc"
	"github.com/gagliardetto/solana-go"
	"github.com/mr-tron/base58"
)

type GetAccountInfoConfig struct {
	Commitment     string
	EncodingType   *int
	DataSlice      *GetAccountInfoDataSlice
	MinContextSlot uint64
}

type GetAccountInfoDataSlice struct {
	Len    *uint64
	Offset *uint64
}

type GetAccountInfoRespContext struct {
	ApiVersion string `json:"apiVersion"`
	Slot       uint64 `json:"slot"`
}

type GetAccountInfoRespValue struct {
	Data       any    `json:"data"`
	Executable bool   `json:"executable"`
	Lamports   uint64 `json:"lamports"`
	Owner      string `json:"owner"`
	RentEpoch  uint64 `json:"rentEpoch"`
	Space      uint64 `json:"space"`
}

type GetAccountInfoResp struct {
	Context GetAccountInfoRespContext `json:"context"`
	Value   *GetAccountInfoRespValue  `json:"value"`
}

const (
	GetAccountEncodingBase58 = iota
	GetAccountEncodingBase64
	GetAccountEncodingBase64Zstd
	GetAccountEncodingJson
)

func (rpcServer *RpcServer) GetAccountInfo(ctx context.Context, p jsonrpc.RawParams) (GetAccountInfoResp, error) {
	params, err := jsonrpc.DecodeParams[[]interface{}](p)
	if err != nil {
		return GetAccountInfoResp{}, fmt.Errorf("decoding params: %w", err)
	}

	if len(params) < 1 {
		return GetAccountInfoResp{}, fmt.Errorf("getAccountInfo requires a string as first parameter")
	}

	pkStr, ok := params[0].(string)
	if !ok {
		return GetAccountInfoResp{}, fmt.Errorf("getAccountInfo requires a string as first parameter")
	}

	pk, err := solana.PublicKeyFromBase58(pkStr)
	if err != nil {
		return GetAccountInfoResp{}, fmt.Errorf("invalid base58 encoding")
	}

	conf, err := parseGetAccountInfoConfMap(params)
	if err != nil {
		return GetAccountInfoResp{}, err
	}
	if err := checkSupportedCommitment(conf.Commitment); err != nil {
		return GetAccountInfoResp{}, err
	}

	// One published bank describes the whole answer. Using a separate global
	// slot could label account bytes with a different point in replay.
	slotCtx := rpcServer.getSlotCtx()
	if slotCtx == nil {
		return GetAccountInfoResp{}, fmt.Errorf("node is not ready to provide account information")
	}
	contextSlot := slotCtx.Slot
	if conf.MinContextSlot > contextSlot {
		// The typed error, not fmt.Errorf: this carries Agave's reserved -32016
		// so a client sees the same code getBlockHeight and getLatestBlockhash
		// already return. minContextSlot means "retry, I am behind" — a generic
		// error makes it indistinguishable from a permanent failure, and the
		// callers that read it are retry loops.
		return GetAccountInfoResp{}, &MinContextSlotNotReachedError{ContextSlot: conf.MinContextSlot}
	}
	respContext := GetAccountInfoRespContext{ApiVersion: apiVersion, Slot: contextSlot}

	acct, err := rpcServer.getAccountAt(slotCtx, pk)
	switch {
	case errors.Is(err, accountsdb.ErrNoAccount):
		// A missing account is context plus a null value, per Solana's shape.
		return GetAccountInfoResp{Context: respContext}, nil
	case err != nil:
		// A read failure is NOT a missing account. Returning a clean negative
		// here would let a caller act on "no balance" during a disk fault.
		return GetAccountInfoResp{}, fmt.Errorf("reading account: %w", err)
	case acct == nil:
		return GetAccountInfoResp{Context: respContext}, nil
	}

	acctData, err := encodeAcctDataWithConfig(acct.Data, conf)
	if err != nil {
		return GetAccountInfoResp{}, err
	}

	val := &GetAccountInfoRespValue{
		Data:       acctData,
		Executable: acct.Executable,
		Lamports:   acct.Lamports,
		Owner:      base58.Encode(acct.Owner[:]),
		RentEpoch:  acct.RentEpoch,
		Space:      uint64(len(acct.Data))}

	return GetAccountInfoResp{Context: respContext, Value: val}, nil
}

// apiVersion labels every response context.
const apiVersion = "mithril 0.1"

// getAccountAt reads the account the published bank actually describes.
//
// The response is labelled with the published bank's slot, so the bytes have
// to come from what that bank sees. Reading the accounts store directly is not
// the same thing: in rooted-durable mode the store holds only rooted state
// (StoreAccounts is a deliberate no-op there), and slots that are replayed but
// not yet rooted live in the bank's own overlay. Going straight to the store
// would return older bytes under a newer slot number — a label that overstates
// what was read, which is precisely the kind of claim this server exists to
// avoid making.
//
// The test seam still wins when installed, so the missing-versus-failed
// distinction stays exercisable.
func (rpcServer *RpcServer) getAccountAt(
	slotCtx *sealevel.SlotCtx, pubkey solana.PublicKey,
) (*accounts.Account, error) {
	if rpcServer.readAccount != nil {
		return rpcServer.readAccount(slotCtx.Slot, pubkey)
	}
	return slotCtx.GetAccountFromAccountsDb(pubkey)
}

// checkSupportedCommitment rejects commitments this node cannot prove. It
// replays and can describe its own processed state; it observes no cluster
// confirmation or finality, so answering those with processed data would be a
// silent lie to a caller who asked precisely because they cared.
func checkSupportedCommitment(commitment string) error {
	switch commitment {
	case "", "processed":
		return nil
	default:
		return fmt.Errorf(
			"commitment %q is not supported by this node; only \"processed\" is available",
			commitment)
	}
}

func parseGetAccountInfoConfMap(params []interface{}) (*GetAccountInfoConfig, error) {
	if len(params) > 2 {
		return nil, &InvalidParamsError{Message: "getAccountInfo accepts at most two parameters"}
	}
	if len(params) < 2 {
		return &GetAccountInfoConfig{}, nil
	}

	var err error
	confMap, ok := params[1].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid config object")
	}

	conf := &GetAccountInfoConfig{}

	// commitment is optional
	commitmentObj, ok := confMap["commitment"]
	if ok {
		commitmentStr, ok := commitmentObj.(string)
		if !ok {
			return nil, fmt.Errorf("invalid commitment")
		}
		conf.Commitment = commitmentStr
	}

	// encoding type is optional. defaults to base58.
	encodingObj, ok := confMap["encoding"]
	if ok {
		encodingStr, ok := encodingObj.(string)
		if ok {
			var encodingType int
			encodingType, err = parseGetAcctDataEncodingType(encodingStr)
			if err != nil {
				return nil, err
			}
			conf.EncodingType = &encodingType
		} else {
			return nil, fmt.Errorf("invalid encoding type")
		}
	}

	// minContextSlot is an optional freshness gate.
	if minSlotObj, ok := confMap["minContextSlot"]; ok {
		minSlot, err := parseExactJSONUint(minSlotObj, "getAccountInfo", "minContextSlot")
		if err != nil {
			return nil, err
		}
		conf.MinContextSlot = minSlot
	}

	// dataSlice is optional. if present, it must contain both 'length' and 'offset' fields.
	dataSliceObj, ok := confMap["dataSlice"]
	if ok {
		dsMap, ok := dataSliceObj.(map[string]interface{})
		if ok {
			conf.DataSlice, err = parseDataSlice(dsMap)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("invalid dataSlice")
		}
	}

	return conf, nil
}

func parseDataSlice(dataSlice map[string]interface{}) (*GetAccountInfoDataSlice, error) {
	lengthObj, hasLen := dataSlice["length"]
	offsetObj, hasOffset := dataSlice["offset"]

	if !hasLen {
		return nil, fmt.Errorf("missing dataSlice field length")
	}
	if !hasOffset {
		return nil, fmt.Errorf("missing dataSlice field offset")
	}

	ds := &GetAccountInfoDataSlice{}
	length, err := parseExactJSONUint(lengthObj, "getAccountInfo", "dataSlice.length")
	if err != nil {
		return nil, err
	}
	offset, err := parseExactJSONUint(offsetObj, "getAccountInfo", "dataSlice.offset")
	if err != nil {
		return nil, err
	}
	ds.Len = &length
	ds.Offset = &offset

	return ds, nil
}

func parseGetAcctDataEncodingType(encodingStr string) (int, error) {
	switch encodingStr {
	case "base58":
		return GetAccountEncodingBase58, nil

	case "base64":
		return GetAccountEncodingBase64, nil

	case "base64+zstd":
		return GetAccountEncodingBase64Zstd, nil

	case "jsonParsed":
		return GetAccountEncodingBase64, nil

	default:
		return 0, fmt.Errorf("invalid data encoding %s", encodingStr)
	}
}

func encodeAcctDataWithConfig(data []byte, config *GetAccountInfoConfig) (interface{}, error) {
	var offset uint64
	var length uint64

	length = uint64(len(data))
	if config.DataSlice != nil {
		if config.DataSlice.Len != nil {
			length = *config.DataSlice.Len
		}

		if config.DataSlice.Offset != nil {
			offset = *config.DataSlice.Offset
		}
	}

	var encodingTypeStr string
	var dataStr string
	var dataObj interface{}

	if config.EncodingType != nil {
		switch *config.EncodingType {
		case GetAccountEncodingBase58:
			{
				encodingTypeStr = "base58"
				acctData := extractRequestedAcctData(data, length, offset)
				if len(acctData) != 0 {
					dataStr = base58.Encode(acctData)
				}
			}

		case GetAccountEncodingJson:
			{
				if config.DataSlice != nil {
					return nil, fmt.Errorf("cannot use jsonParsed with dataSlice")
				}
			}

			fallthrough
		case GetAccountEncodingBase64:
			{
				encodingTypeStr = "base64"
				acctData := extractRequestedAcctData(data, length, offset)
				if len(acctData) != 0 {
					dataStr = base64.StdEncoding.EncodeToString(acctData)
				}
			}

		case GetAccountEncodingBase64Zstd:
			{
				encodingTypeStr = "base64+zstd"
				acctData := extractRequestedAcctData(data, length, offset)
				compressedBytes, _ := zstd.Compress(nil, acctData)
				dataStr = base64.StdEncoding.EncodeToString(compressedBytes)
			}
		}

		dataObj = []string{dataStr, encodingTypeStr}
	} else {
		acctData := extractRequestedAcctData(data, length, offset)
		if len(acctData) != 0 {
			dataObj = base58.Encode(acctData)
		}
	}

	return dataObj, nil
}

func extractRequestedAcctData(data []byte, length uint64, offset uint64) []byte {
	if offset > uint64(len(data)) {
		return nil
	}

	end := safemath.SaturatingAddU64(length, offset)
	if end > uint64(len(data)) {
		return data[offset:]
	}

	return data[offset : offset+length]
}
