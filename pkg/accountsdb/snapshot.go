package accountsdb

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/gagliardetto/solana-go"
)

// SnapshotManifest contains metadata about a shrunken AccountsDb snapshot.
type SnapshotManifest struct {
	Slot         uint64   `json:"slot"`
	AccountCount int      `json:"account_count"`
	Accounts     []string `json:"accounts"` // List of account pubkeys (base58)
}

// AccountEntry represents a single account in the snapshot.
type AccountEntry struct {
	Pubkey     string `json:"pubkey"`
	Lamports   uint64 `json:"lamports"`
	Owner      string `json:"owner"`
	Executable bool   `json:"executable"`
	RentEpoch  uint64 `json:"rent_epoch"`
	DataLen    int    `json:"data_len"`
}

// ExportAccountsToTar exports a subset of accounts from the AccountsDb to a tar.gz file.
// The tar contains:
// - manifest.json: metadata about the snapshot
// - accounts.json: account metadata (pubkey, lamports, owner, etc.)
// - data/<pubkey>.bin: raw account data for each account
func ExportAccountsToTar(db *AccountsDb, slot uint64, pubkeys []solana.PublicKey, outputPath string) error {
	file, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer file.Close()

	gzWriter := gzip.NewWriter(file)
	defer gzWriter.Close()

	tarWriter := tar.NewWriter(gzWriter)
	defer tarWriter.Close()

	var accountEntries []AccountEntry
	var pubkeyStrings []string
	accountData := make(map[string][]byte)

	// Fetch all accounts
	for _, pk := range pubkeys {
		acct, err := db.GetAccount(slot, pk)
		if err != nil {
			// Skip accounts that don't exist
			continue
		}

		pkStr := pk.String()
		pubkeyStrings = append(pubkeyStrings, pkStr)

		accountEntries = append(accountEntries, AccountEntry{
			Pubkey:     pkStr,
			Lamports:   acct.Lamports,
			Owner:      acct.Owner.String(),
			Executable: acct.Executable,
			RentEpoch:  acct.RentEpoch,
			DataLen:    len(acct.Data),
		})

		if len(acct.Data) > 0 {
			accountData[pkStr] = acct.Data
		}
	}

	// Write manifest
	manifest := SnapshotManifest{
		Slot:         slot,
		AccountCount: len(accountEntries),
		Accounts:     pubkeyStrings,
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}
	if err := writeTarFile(tarWriter, "manifest.json", manifestBytes); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}

	// Write account metadata
	accountsBytes, err := json.MarshalIndent(accountEntries, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal accounts: %w", err)
	}
	if err := writeTarFile(tarWriter, "accounts.json", accountsBytes); err != nil {
		return fmt.Errorf("failed to write accounts: %w", err)
	}

	// Write account data files
	for pkStr, data := range accountData {
		filename := fmt.Sprintf("data/%s.bin", pkStr)
		if err := writeTarFile(tarWriter, filename, data); err != nil {
			return fmt.Errorf("failed to write account data for %s: %w", pkStr, err)
		}
	}

	return nil
}

func writeTarFile(tw *tar.Writer, name string, data []byte) error {
	header := &tar.Header{
		Name: name,
		Mode: 0644,
		Size: int64(len(data)),
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// ImportAccountsFromTar loads accounts from a tar.gz snapshot and creates an AccountsDb.
func ImportAccountsFromTar(tarPath string, dbDir string) (*AccountsDb, error) {
	file, err := os.Open(tarPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open tar file: %w", err)
	}
	defer file.Close()

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)

	var manifest SnapshotManifest
	var accountEntries []AccountEntry
	accountData := make(map[string][]byte)

	// Read all files from tar
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar header: %w", err)
		}

		data, err := io.ReadAll(tarReader)
		if err != nil {
			return nil, fmt.Errorf("failed to read tar file %s: %w", header.Name, err)
		}

		switch header.Name {
		case "manifest.json":
			if err := json.Unmarshal(data, &manifest); err != nil {
				return nil, fmt.Errorf("failed to unmarshal manifest: %w", err)
			}
		case "accounts.json":
			if err := json.Unmarshal(data, &accountEntries); err != nil {
				return nil, fmt.Errorf("failed to unmarshal accounts: %w", err)
			}
		default:
			// Account data file: data/<pubkey>.bin
			if len(header.Name) > 5 && header.Name[:5] == "data/" {
				pkStr := header.Name[5 : len(header.Name)-4] // Remove "data/" prefix and ".bin" suffix
				accountData[pkStr] = data
			}
		}
	}

	// Build account list
	accts := make([]*accounts.Account, 0, len(accountEntries))
	for _, entry := range accountEntries {
		pk, err := solana.PublicKeyFromBase58(entry.Pubkey)
		if err != nil {
			return nil, fmt.Errorf("invalid pubkey %s: %w", entry.Pubkey, err)
		}

		owner, err := solana.PublicKeyFromBase58(entry.Owner)
		if err != nil {
			return nil, fmt.Errorf("invalid owner %s: %w", entry.Owner, err)
		}

		var data []byte
		if d, ok := accountData[entry.Pubkey]; ok {
			data = d
		}

		acct := &accounts.Account{
			Slot:       manifest.Slot,
			Key:        pk,
			Lamports:   entry.Lamports,
			Data:       data,
			Owner:      owner,
			Executable: entry.Executable,
			RentEpoch:  entry.RentEpoch,
		}
		accts = append(accts, acct)
	}

	// Create AccountsDb from accounts
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create db dir: %w", err)
	}

	db, err := CreateAccountsDbFromSnapshot(accts, manifest.Slot, dbDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create AccountsDb: %w", err)
	}

	return db, nil
}

// ExportAccountsToDir exports accounts to a directory structure (non-tar version).
// Useful for debugging or when you want to inspect individual accounts.
func ExportAccountsToDir(db *AccountsDb, slot uint64, pubkeys []solana.PublicKey, outputDir string) error {
	if err := os.MkdirAll(filepath.Join(outputDir, "data"), 0755); err != nil {
		return fmt.Errorf("failed to create output dir: %w", err)
	}

	var accountEntries []AccountEntry
	var pubkeyStrings []string

	// Fetch and write all accounts
	for _, pk := range pubkeys {
		acct, err := db.GetAccount(slot, pk)
		if err != nil {
			continue
		}

		pkStr := pk.String()
		pubkeyStrings = append(pubkeyStrings, pkStr)

		accountEntries = append(accountEntries, AccountEntry{
			Pubkey:     pkStr,
			Lamports:   acct.Lamports,
			Owner:      acct.Owner.String(),
			Executable: acct.Executable,
			RentEpoch:  acct.RentEpoch,
			DataLen:    len(acct.Data),
		})

		if len(acct.Data) > 0 {
			dataPath := filepath.Join(outputDir, "data", pkStr+".bin")
			if err := os.WriteFile(dataPath, acct.Data, 0644); err != nil {
				return fmt.Errorf("failed to write account data for %s: %w", pkStr, err)
			}
		}
	}

	// Write manifest
	manifest := SnapshotManifest{
		Slot:         slot,
		AccountCount: len(accountEntries),
		Accounts:     pubkeyStrings,
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "manifest.json"), manifestBytes, 0644); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}

	// Write account metadata
	accountsBytes, err := json.MarshalIndent(accountEntries, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal accounts: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "accounts.json"), accountsBytes, 0644); err != nil {
		return fmt.Errorf("failed to write accounts: %w", err)
	}

	return nil
}
