package accountsdb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	bin "github.com/gagliardetto/binary"
)

// The redo log makes a per-slot commit crash-safe: accounts are staged here durably
// before apply, and recovery re-applies the record (idempotent — absolute values)
// after a mid-apply crash. Deleted only once the superseding store is durable.
const (
	redoDirName    = "redo"
	redoFileSuffix = ".redo"
	redoMagic      = 0x4d52444f // "MRDO"
	redoVersion    = 1
)

// ErrTornRedo means a redo file failed its checksum — a partial/torn write left
// by a crash during WriteRedo. It names no committed state and is discarded.
var ErrTornRedo = fmt.Errorf("accountsdb: torn redo file (checksum mismatch)")

func redoDir(acctsDir string) string { return filepath.Join(acctsDir, redoDirName) }

func redoPath(acctsDir string, slot uint64) string {
	return filepath.Join(redoDir(acctsDir), strconv.FormatUint(slot, 10)+redoFileSuffix)
}

// WriteRedo durably stages slot's accounts + bankhash as a crc32-framed commit
// record via temp-write + fsync + rename + dir fsync. Nil entries are skipped.
func WriteRedo(acctsDir string, slot uint64, bankhash []byte, accts []*accounts.Account) error {
	dir := redoDir(acctsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("accountsdb: mkdir redo: %w", err)
	}

	live := make([]*accounts.Account, 0, len(accts))
	for _, a := range accts {
		if a != nil {
			live = append(live, a)
		}
	}

	var body bytes.Buffer
	enc := bin.NewBinEncoder(&body)
	_ = enc.WriteUint32(redoMagic, bin.LE)
	_ = enc.WriteUint32(redoVersion, bin.LE)
	_ = enc.WriteUint64(slot, bin.LE)
	_ = enc.WriteUint32(uint32(len(bankhash)), bin.LE)
	_ = enc.WriteBytes(bankhash, false)
	_ = enc.WriteUint64(uint64(len(live)), bin.LE)
	for _, a := range live {
		if err := a.MarshalWithEncoder(enc); err != nil {
			return fmt.Errorf("accountsdb: encode redo acct: %w", err)
		}
	}
	var crcBuf [4]byte
	binary.LittleEndian.PutUint32(crcBuf[:], crc32.ChecksumIEEE(body.Bytes()))

	tmp := redoPath(acctsDir, slot) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("accountsdb: create redo tmp: %w", err)
	}
	if _, err := f.Write(body.Bytes()); err != nil {
		f.Close()
		return fmt.Errorf("accountsdb: write redo body: %w", err)
	}
	if _, err := f.Write(crcBuf[:]); err != nil {
		f.Close()
		return fmt.Errorf("accountsdb: write redo crc: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("accountsdb: fsync redo tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("accountsdb: close redo tmp: %w", err)
	}
	if err := os.Rename(tmp, redoPath(acctsDir, slot)); err != nil {
		return fmt.Errorf("accountsdb: rename redo: %w", err)
	}
	return fsyncDir(dir)
}

// ReadRedo loads a staged redo record (accounts + bankhash), verifying its
// checksum. A torn or malformed file returns ErrTornRedo.
func ReadRedo(acctsDir string, slot uint64) ([]*accounts.Account, []byte, error) {
	data, err := os.ReadFile(redoPath(acctsDir, slot))
	if err != nil {
		return nil, nil, err
	}
	if len(data) < 4 {
		return nil, nil, ErrTornRedo
	}
	body, crcBytes := data[:len(data)-4], data[len(data)-4:]
	if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(crcBytes) {
		return nil, nil, ErrTornRedo
	}

	dec := bin.NewBinDecoder(body)
	if magic, err := dec.ReadUint32(bin.LE); err != nil || magic != redoMagic {
		return nil, nil, ErrTornRedo
	}
	if ver, err := dec.ReadUint32(bin.LE); err != nil || ver != redoVersion {
		return nil, nil, fmt.Errorf("accountsdb: unsupported redo version (err=%v)", err)
	}
	if gotSlot, err := dec.ReadUint64(bin.LE); err != nil || gotSlot != slot {
		return nil, nil, fmt.Errorf("accountsdb: redo slot mismatch (file=%d want=%d err=%v)", gotSlot, slot, err)
	}
	bankhashLen, err := dec.ReadUint32(bin.LE)
	if err != nil || uint64(bankhashLen) > uint64(dec.Remaining()) {
		return nil, nil, ErrTornRedo
	}
	bankhash, err := dec.ReadNBytes(int(bankhashLen))
	if err != nil {
		return nil, nil, ErrTornRedo
	}
	count, err := dec.ReadUint64(bin.LE)
	if err != nil {
		return nil, nil, ErrTornRedo
	}
	out := make([]*accounts.Account, 0, count)
	for range count {
		var a accounts.Account
		if err := a.UnmarshalWithDecoder(dec); err != nil {
			return nil, nil, ErrTornRedo
		}
		out = append(out, &a)
	}
	return out, bankhash, nil
}

// ListPendingRedo returns, ascending, the slots that have staged redo files —
// an interrupted commit to recover on startup.
func ListPendingRedo(acctsDir string) ([]uint64, error) {
	entries, err := os.ReadDir(redoDir(acctsDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var slots []uint64
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, redoFileSuffix) {
			continue // skip *.tmp and anything else
		}
		s, err := strconv.ParseUint(strings.TrimSuffix(name, redoFileSuffix), 10, 64)
		if err != nil {
			continue
		}
		slots = append(slots, s)
	}
	slices.Sort(slots)
	return slots, nil
}

// quarantineRedo renames a torn/unreadable redo aside (.corrupt) so recovery can
// continue without re-listing it, while preserving it for inspection.
func quarantineRedo(acctsDir string, slot uint64) error {
	src := redoPath(acctsDir, slot)
	if err := os.Rename(src, src+".corrupt"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return fsyncDir(redoDir(acctsDir))
}

// DeleteRedo removes a finalized slot's redo file and fsyncs the directory.
func DeleteRedo(acctsDir string, slot uint64) error {
	if err := os.Remove(redoPath(acctsDir, slot)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return fsyncDir(redoDir(acctsDir))
}

// fsyncDir flushes a directory entry change to stable storage. Required on Linux:
// fsync of a file does not make its directory entry durable.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
