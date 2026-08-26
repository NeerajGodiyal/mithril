package fees

import (
	"errors"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

// NonceStateSize is Agave's solana_nonce::state::State::size().
const NonceStateSize = 80

var (
	ErrFeePayerNotFound         = errors.New("account not found")
	ErrInvalidAccountForFee     = errors.New("invalid account for fee")
	ErrInsufficientFundsForFee  = sealevel.InstrErrInsufficientFunds
	ErrInsufficientFundsForRent = errors.New("insufficient funds for rent")
	ErrFeePayerSource           = errors.New("fee payer account source failure")
)

const (
	systemAccountKindSystem = iota
	systemAccountKindNonce
)

// RentForSlot returns the bank-local Rent sysvar, then the process cache,
// then Agave's default (890880 lamports for a 0-byte account).
func RentForSlot(slotCtx *sealevel.SlotCtx) sealevel.SysvarRent {
	if slotCtx != nil {
		if bankSysvars := slotCtx.BankSysvars(); bankSysvars != nil {
			if rent, ok := bankSysvars.Rent(); ok {
				return rent
			}
		}
	}
	if sealevel.SysvarCache.Rent.Sysvar != nil {
		return *sealevel.SysvarCache.Rent.Sysvar
	}
	return sealevel.NewDefaultRentSysvar()
}

// ValidateFeePayer matches Agave svm::account_loader::validate_fee_payer
// without mutating the account: the payer must exist, be a system or nonce
// account, and cover the fee (plus the nonce rent reserve). A rent-exempt
// system payer may be drained to zero, but must not be left with a
// nonzero balance below the exemption minimum.
func ValidateFeePayer(payer *accounts.Account, fee uint64, rent sealevel.SysvarRent) error {
	if payer == nil || payer.Lamports == 0 {
		return ErrFeePayerNotFound
	}
	kind, ok := systemAccountKind(payer)
	if !ok {
		return ErrInvalidAccountForFee
	}
	minBalance := uint64(0)
	if kind == systemAccountKindNonce {
		minBalance = rent.MinimumBalance(NonceStateSize)
	}
	if payer.Lamports < minBalance || payer.Lamports-minBalance < fee {
		return ErrInsufficientFundsForFee
	}
	post := payer.Lamports - fee
	if kind == systemAccountKindSystem && post != 0 && rent.IsExempt(payer.Lamports, 0) && !rent.IsExempt(post, 0) {
		return ErrInsufficientFundsForRent
	}
	return nil
}

// PayerCanFund reports whether the current in-slot fee payer can pay this
// transaction's fee and still satisfy ValidateFeePayer. It does not execute.
func PayerCanFund(slotCtx *sealevel.SlotCtx, tx *solana.Transaction) error {
	if tx == nil || len(tx.Message.AccountKeys) == 0 {
		return ErrFeePayerNotFound
	}
	payer, err := loadPayer(slotCtx, tx.Message.AccountKeys[0])
	if err != nil {
		if !errors.Is(err, accountsdb.ErrNoAccount) && !errors.Is(err, ErrFeePayerNotFound) {
			return errors.Join(ErrFeePayerSource, err)
		}
		return ErrFeePayerNotFound
	}
	feats := features.NewFeaturesDefault()
	if slotCtx != nil && slotCtx.Features != nil {
		feats = slotCtx.Features
	}
	instrs, err := feeInstructions(tx)
	if err != nil {
		return err
	}
	limits, err := sealevel.ComputeBudgetForTransaction(tx, instrs, feats)
	if err != nil {
		return err
	}
	feeInfo := CalculateTxFees(tx, instrs, limits, feats)
	return ValidateFeePayer(payer, feeInfo.TotalFee, RentForSlot(slotCtx))
}

func loadPayer(slotCtx *sealevel.SlotCtx, pk solana.PublicKey) (*accounts.Account, error) {
	if slotCtx == nil {
		return nil, ErrFeePayerNotFound
	}
	if acct, err := slotCtx.GetAccountShared(pk); err == nil && acct != nil {
		return acct, nil
	}
	if slotCtx.UnrootedRead == nil && slotCtx.AccountsDb == nil {
		return nil, ErrFeePayerNotFound
	}
	return slotCtx.GetAccountFromAccountsDb(pk)
}

func feeInstructions(tx *solana.Transaction) ([]sealevel.Instruction, error) {
	out := make([]sealevel.Instruction, 0, len(tx.Message.Instructions))
	for _, compiled := range tx.Message.Instructions {
		programID, err := tx.ResolveProgramIDIndex(compiled.ProgramIDIndex)
		if err != nil {
			return nil, err
		}
		out = append(out, sealevel.Instruction{
			ProgramId: programID,
			Data:      compiled.Data,
		})
	}
	return out, nil
}

func systemAccountKind(acct *accounts.Account) (int, bool) {
	if acct.Owner != a.SystemProgramAddr {
		return 0, false
	}
	if len(acct.Data) == 0 {
		return systemAccountKindSystem, true
	}
	nonceState, err := sealevel.UnmarshalNonceStateVersions(acct.Data)
	if err != nil || !nonceState.State().IsInitialized {
		return 0, false
	}
	return systemAccountKindNonce, true
}
