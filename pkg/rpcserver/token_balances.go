package rpcserver

import (
	"encoding/binary"
	"strconv"
	"strings"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/gagliardetto/solana-go"
)

// SPL Token program IDs.
var (
	splTokenProgramID     = solana.MustPublicKeyFromBase58("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")
	splToken2022ProgramID = solana.MustPublicKeyFromBase58("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb")
)

// SPL Token Account is exactly 165 bytes; offsets are stable across both
// the legacy program and Token-2022 (Token-2022 stores extension data
// past byte 165, but the leading account state has the same layout).
const (
	tokenAccountSize         = 165
	tokenAccountMintOffset   = 0
	tokenAccountOwnerOffset  = 32
	tokenAccountAmountOffset = 64
	tokenAccountStateOffset  = 108
	// Mint account layout: 82 bytes for legacy SPL Token. decimals lives at
	// offset 44 (mintAuthorityOption 4 + mintAuthority 32 + supply 8 = 44).
	mintAccountSize       = 82
	mintDecimalsOffset    = 44
	mintInitializedOffset = 45
	tokenMultisigSize     = 355
	token2022TypeOffset   = 165
	token2022MintType     = 1
	token2022AccountType  = 2
)

type strictTokenAccount struct {
	mint      solana.PublicKey
	owner     solana.PublicKey
	programID solana.PublicKey
	amount    uint64
}

type strictTokenMint struct {
	programID solana.PublicKey
	decimals  uint8
}

// isTokenProgramOwner reports whether an account is owned by either the
// legacy SPL Token program or Token-2022. Both programs use the same
// account layout for the leading 165 bytes.
func isTokenProgramOwner(owner [32]byte) bool {
	return owner == [32]byte(splTokenProgramID) || owner == [32]byte(splToken2022ProgramID)
}

func validCOption(data []byte, offset int) bool {
	if offset < 0 || offset+4 > len(data) {
		return false
	}
	tag := binary.LittleEndian.Uint32(data[offset : offset+4])
	return tag == 0 || tag == 1
}

// validToken2022Envelope checks the base/type boundary while leaving TLV
// extension contents to the Token-2022 program.
func validToken2022Envelope(data []byte, baseSize int, accountType byte) bool {
	if len(data) == baseSize {
		return true
	}
	if len(data) == tokenMultisigSize || len(data) <= token2022TypeOffset {
		return false
	}
	for _, b := range data[baseSize:token2022TypeOffset] {
		if b != 0 {
			return false
		}
	}
	return data[token2022TypeOffset] == accountType
}

func parseStrictTokenAccount(acct *accounts.Account) (strictTokenAccount, bool) {
	if acct == nil || !isTokenProgramOwner(acct.Owner) || len(acct.Data) < tokenAccountSize {
		return strictTokenAccount{}, false
	}
	is2022 := acct.Owner == [32]byte(splToken2022ProgramID)
	if (!is2022 && len(acct.Data) != tokenAccountSize) ||
		(is2022 && !validToken2022Envelope(acct.Data, tokenAccountSize, token2022AccountType)) {
		return strictTokenAccount{}, false
	}
	if !validCOption(acct.Data, 72) || !validCOption(acct.Data, 109) || !validCOption(acct.Data, 129) {
		return strictTokenAccount{}, false
	}
	state := acct.Data[tokenAccountStateOffset]
	if state != 1 && state != 2 {
		return strictTokenAccount{}, false
	}
	programID := splTokenProgramID
	if is2022 {
		programID = splToken2022ProgramID
	}
	return strictTokenAccount{
		mint:      publicKeyFromOffset(acct.Data, tokenAccountMintOffset),
		owner:     publicKeyFromOffset(acct.Data, tokenAccountOwnerOffset),
		programID: programID,
		amount:    binary.LittleEndian.Uint64(acct.Data[tokenAccountAmountOffset : tokenAccountAmountOffset+8]),
	}, true
}

func parseStrictTokenMint(acct *accounts.Account, programID solana.PublicKey) (uint8, bool) {
	if acct == nil || acct.Owner != [32]byte(programID) || len(acct.Data) < mintAccountSize {
		return 0, false
	}
	is2022 := programID == splToken2022ProgramID
	if (!is2022 && len(acct.Data) != mintAccountSize) ||
		(is2022 && !validToken2022Envelope(acct.Data, mintAccountSize, token2022MintType)) {
		return 0, false
	}
	if !validCOption(acct.Data, 0) || !validCOption(acct.Data, 46) || acct.Data[mintInitializedOffset] != 1 {
		return 0, false
	}
	return acct.Data[mintDecimalsOffset], true
}

func transactionTokenMetadata(tx *solana.Transaction) (bool, map[int]struct{}) {
	if tx == nil {
		return false, nil
	}
	hasTokenProgram := false
	for _, key := range tx.Message.AccountKeys {
		if key == splTokenProgramID || key == splToken2022ProgramID {
			hasTokenProgram = true
			break
		}
	}
	invoked := make(map[int]struct{}, len(tx.Message.Instructions))
	for _, instruction := range tx.Message.Instructions {
		invoked[int(instruction.ProgramIDIndex)] = struct{}{}
	}
	return hasTokenProgram, invoked
}

func tokenBalancesForTransaction(
	tx *solana.Transaction,
	txAccts []*accounts.Account,
	read func(solana.PublicKey) (*accounts.Account, error),
) []TokenBalancePayload {
	if txAccts == nil {
		return nil
	}
	if len(txAccts) == 0 {
		return []TokenBalancePayload{}
	}
	hasTokenProgram, invoked := transactionTokenMetadata(tx)
	if !hasTokenProgram {
		return []TokenBalancePayload{}
	}

	mints := make(map[solana.PublicKey]strictTokenMint)
	snapshot := make(map[solana.PublicKey]*accounts.Account, len(txAccts))
	for idx, acct := range txAccts {
		if idx < len(tx.Message.AccountKeys) {
			snapshot[tx.Message.AccountKeys[idx]] = acct
		}
	}
	for _, acct := range txAccts {
		parsed, ok := parseStrictTokenAccount(acct)
		if !ok {
			continue
		}
		if prior, ok := mints[parsed.mint]; ok && prior.programID == parsed.programID {
			continue
		}
		mintAcct, exists := snapshot[parsed.mint]
		if !exists {
			if read == nil {
				continue
			}
			var err error
			mintAcct, err = read(parsed.mint)
			if err != nil {
				continue
			}
		}
		decimals, ok := parseStrictTokenMint(mintAcct, parsed.programID)
		if ok {
			mints[parsed.mint] = strictTokenMint{programID: parsed.programID, decimals: decimals}
		}
	}

	out := make([]TokenBalancePayload, 0)
	for idx, acct := range txAccts {
		if idx >= len(tx.Message.AccountKeys) {
			break
		}
		if _, ok := invoked[idx]; ok || tx.Message.AccountKeys[idx] == splTokenProgramID || tx.Message.AccountKeys[idx] == splToken2022ProgramID {
			continue
		}
		parsed, ok := parseStrictTokenAccount(acct)
		if !ok {
			continue
		}
		mint, ok := mints[parsed.mint]
		if !ok || mint.programID != parsed.programID {
			continue
		}
		out = append(out, TokenBalancePayload{
			AccountIndex: uint8(idx),
			Mint:         parsed.mint.String(),
			Owner:        parsed.owner.String(),
			ProgramId:    parsed.programID.String(),
			UiTokenAmount: UiTokenAmountPayload{
				Amount:         strconv.FormatUint(parsed.amount, 10),
				Decimals:       mint.decimals,
				UiAmount:       uiAmountForRaw(parsed.amount, mint.decimals),
				UiAmountString: uiAmountStringForRaw(parsed.amount, mint.decimals),
			},
		})
	}
	return out
}

func publicKeyFromOffset(data []byte, off int) solana.PublicKey {
	var pk solana.PublicKey
	copy(pk[:], data[off:off+32])
	return pk
}

// uiAmountForRaw returns the raw token amount divided by 10^decimals as
// a float64. Matches Agave's lossy float conversion; precision suffers
// for amounts > 2^53 but this is the wire contract.
func uiAmountForRaw(amount uint64, decimals uint8) *float64 {
	if decimals == 0 {
		v := float64(amount)
		return &v
	}
	v := float64(amount) / pow10Float(decimals)
	return &v
}

// uiAmountStringForRaw renders amount/10^decimals as a decimal string
// with trailing zeros trimmed, matching Agave's UiAmountString. Pure
// string manipulation so precision is preserved for any decimals up to
// uint8 max (255) — Token-2022 doesn't bound decimals at the protocol
// level, and float64-based division saturates uint64 cast at decimals≥20.
func uiAmountStringForRaw(amount uint64, decimals uint8) string {
	if amount == 0 {
		return "0"
	}
	digits := strconv.FormatUint(amount, 10)
	if decimals == 0 {
		return digits
	}
	d := int(decimals)
	if d >= len(digits) {
		// Whole part is zero; pad fractional with leading zeros.
		frac := strings.Repeat("0", d-len(digits)) + digits
		frac = strings.TrimRight(frac, "0")
		if frac == "" {
			return "0"
		}
		return "0." + frac
	}
	whole := digits[:len(digits)-d]
	frac := strings.TrimRight(digits[len(digits)-d:], "0")
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}

// pow10Float returns 10^n as a float64. Used only by uiAmountForRaw
// (the *float64 path); the string path uses index math instead.
func pow10Float(n uint8) float64 {
	out := 1.0
	for i := uint8(0); i < n; i++ {
		out *= 10
	}
	return out
}
