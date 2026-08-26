package rootedevents

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
)

const (
	// SidecarDirectory is the AccountsDB-rooted location for event batches.
	SidecarDirectory = "rooted-events"
	// SidecarVersion is the current immutable sidecar-reference version.
	SidecarVersion = uint32(2)

	sidecarPrefix  = "events-"
	sidecarSuffix  = ".jsonl"
	sidecarMaxSize = uint64(8 << 30)
	maxEventBytes  = 16 << 20
)

type countingWriter struct {
	n uint64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += uint64(len(p))
	return len(p), nil
}

// PrepareSidecar streams one immutable fold chunk to a content-addressed file.
// The returned reference is not authoritative until a rooted fold manifest
// selects it through its ResumeContext.
func PrepareSidecar(root string, deltas []accounts.SlotDelta, metadata map[uint64]SlotMeta) (*state.RootedEventBatchRef, error) {
	if len(deltas) == 0 {
		return nil, errors.New("prepare rooted events: empty fold chunk")
	}
	dir, err := sidecarDir(root)
	if err != nil {
		return nil, err
	}
	exists, err := rootedEventDirectoryExists(dir)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := os.Mkdir(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create rooted-event directory: %w", err)
		}
		if exists, err = rootedEventDirectoryExists(dir); err != nil || !exists {
			if err == nil {
				err = errors.New("created rooted-event directory is missing")
			}
			return nil, err
		}
		if err := syncDir(filepath.Dir(dir)); err != nil {
			return nil, fmt.Errorf("sync AccountsDB root after rooted-event directory creation: %w", err)
		}
	}

	tmp, err := os.CreateTemp(dir, ".events-*.partial")
	if err != nil {
		return nil, fmt.Errorf("create rooted-event temporary: %w", err)
	}
	tmpPath := tmp.Name()
	tmpOpen := true
	defer func() {
		if tmpOpen {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()

	digest := sha256.New()
	count := &countingWriter{}
	encoder := json.NewEncoder(io.MultiWriter(tmp, digest, count))
	if err := walkEvents(deltas, metadata, func(event Event) error {
		if err := encoder.Encode(&event); err != nil {
			return fmt.Errorf("encode rooted event at cursor %+v: %w", event.Cursor, err)
		}
		if count.n > sidecarMaxSize {
			return fmt.Errorf("rooted-event sidecar exceeds %d-byte limit", sidecarMaxSize)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if count.n == 0 {
		return nil, errors.New("prepare rooted events: encoder produced no records")
	}
	if err := tmp.Chmod(0o444); err != nil {
		return nil, fmt.Errorf("chmod rooted-event temporary: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return nil, fmt.Errorf("sync rooted-event temporary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		tmpOpen = false
		return nil, fmt.Errorf("close rooted-event temporary: %w", err)
	}
	tmpOpen = false

	digestHex := hex.EncodeToString(digest.Sum(nil))
	ref := &state.RootedEventBatchRef{
		Version:     SidecarVersion,
		FromSlot:    deltas[0].Slot,
		ThroughSlot: deltas[len(deltas)-1].Slot,
		File:        sidecarBasename(deltas[0].Slot, deltas[len(deltas)-1].Slot, digestHex),
		Size:        count.n,
		SHA256:      digestHex,
	}
	if err := ValidateSidecarRef(ref); err != nil {
		return nil, err
	}
	final := filepath.Join(dir, ref.File)
	if err := os.Link(tmpPath, final); err != nil {
		if !os.IsExist(err) {
			return nil, fmt.Errorf("install immutable rooted-event sidecar: %w", err)
		}
		if err := ReadSidecar(root, ref, nil); err != nil {
			return nil, fmt.Errorf("existing immutable rooted-event sidecar is invalid: %w", err)
		}
	}
	if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove rooted-event temporary: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return nil, fmt.Errorf("sync rooted-event directory: %w", err)
	}
	return ref, nil
}

// ValidateSidecarRef checks a sidecar reference without filesystem access.
func ValidateSidecarRef(ref *state.RootedEventBatchRef) error {
	if ref == nil {
		return errors.New("rooted-event reference is missing")
	}
	if ref.Version != SidecarVersion {
		return fmt.Errorf("rooted-event version %d is unsupported (want %d)", ref.Version, SidecarVersion)
	}
	if ref.FromSlot > ref.ThroughSlot {
		return fmt.Errorf("rooted-event slot range %d..%d is invalid", ref.FromSlot, ref.ThroughSlot)
	}
	if ref.Size == 0 || ref.Size > sidecarMaxSize {
		return fmt.Errorf("rooted-event size %d is outside allowed range 1..%d", ref.Size, sidecarMaxSize)
	}
	digest, err := hex.DecodeString(ref.SHA256)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != ref.SHA256 {
		return fmt.Errorf("rooted-event SHA-256 %q is not 64 lowercase hexadecimal characters", ref.SHA256)
	}
	want := sidecarBasename(ref.FromSlot, ref.ThroughSlot, ref.SHA256)
	if ref.File != want || filepath.Base(ref.File) != ref.File || filepath.Clean(ref.File) != ref.File {
		return fmt.Errorf("rooted-event filename %q is invalid (want %q)", ref.File, want)
	}
	return nil
}

// ReadSidecar verifies a selected sidecar before streaming decoded records to
// emit. emit may be nil when the caller only needs integrity verification.
func ReadSidecar(root string, ref *state.RootedEventBatchRef, emit func(Event) error) error {
	if err := ValidateSidecarRef(ref); err != nil {
		return err
	}
	dir, err := sidecarDir(root)
	if err != nil {
		return err
	}
	exists, err := rootedEventDirectoryExists(dir)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("rooted-event directory is missing")
	}
	path := filepath.Join(dir, ref.File)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat rooted-event sidecar %q: %w", ref.File, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("rooted-event sidecar %q is not a regular non-symlink file", ref.File)
	}
	if uint64(info.Size()) != ref.Size {
		return fmt.Errorf("rooted-event sidecar %q size %d does not match reference %d", ref.File, info.Size(), ref.Size)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open rooted-event sidecar %q: %w", ref.File, err)
	}
	defer f.Close()

	digest := sha256.New()
	read, err := io.Copy(digest, io.LimitReader(f, int64(ref.Size)+1))
	if err != nil {
		return fmt.Errorf("verify rooted-event sidecar %q: %w", ref.File, err)
	}
	if uint64(read) != ref.Size {
		return fmt.Errorf("rooted-event sidecar %q read %d bytes, want %d", ref.File, read, ref.Size)
	}
	want, _ := hex.DecodeString(ref.SHA256)
	if subtle.ConstantTimeCompare(digest.Sum(nil), want) != 1 {
		return fmt.Errorf("rooted-event sidecar %q SHA-256 mismatch", ref.File)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind rooted-event sidecar %q: %w", ref.File, err)
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), maxEventBytes+1)
	validator := newStreamValidator(ref)
	for scanner.Scan() {
		var event Event
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&event); err != nil {
			return fmt.Errorf("decode rooted-event sidecar %q: %w", ref.File, err)
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			if err == nil {
				err = errors.New("multiple JSON values in one record")
			}
			return fmt.Errorf("decode rooted-event sidecar %q: %w", ref.File, err)
		}
		if err := validator.accept(event); err != nil {
			return fmt.Errorf("validate rooted-event sidecar %q: %w", ref.File, err)
		}
		if emit != nil {
			if err := emit(event); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("decode rooted-event sidecar %q: %w", ref.File, err)
	}
	if err := validator.finish(); err != nil {
		return fmt.Errorf("validate rooted-event sidecar %q: %w", ref.File, err)
	}
	return nil
}

// CleanupSidecars removes partial writes and immutable sidecars not selected
// by the caller's retained fold manifests. Unknown files are left alone.
func CleanupSidecars(root string, keep []*state.RootedEventBatchRef) ([]string, error) {
	dir, err := sidecarDir(root)
	if err != nil {
		return nil, err
	}
	exists, err := rootedEventDirectoryExists(dir)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	keptFiles := make(map[string]struct{}, len(keep))
	for _, ref := range keep {
		if err := ValidateSidecarRef(ref); err != nil {
			return nil, fmt.Errorf("cleanup rooted-event sidecars: invalid keep reference: %w", err)
		}
		keptFiles[ref.File] = struct{}{}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read rooted-event directory: %w", err)
	}
	var removed []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		isPartial := strings.HasPrefix(name, ".events-") && strings.HasSuffix(name, ".partial")
		_, isSidecar := parseSidecarBasename(name)
		if !isPartial && !isSidecar {
			continue
		}
		if _, keep := keptFiles[name]; keep {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return removed, fmt.Errorf("remove unreferenced rooted-event sidecar %q: %w", name, err)
		}
		removed = append(removed, path)
	}
	if len(removed) > 0 {
		if err := syncDir(dir); err != nil {
			return removed, fmt.Errorf("sync rooted-event directory after cleanup: %w", err)
		}
	}
	return removed, nil
}

type streamValidator struct {
	ref              *state.RootedEventBatchRef
	started          bool
	slot             uint64
	nextOrdinal      uint32
	lastAccountKey   solana.PublicKey
	haveAccountKey   bool
	transactionCount uint32
	accountCount     uint32
	accountsStarted  bool
	slotClosed       bool
	lastRoot         uint64
	haveRoot         bool
	lastBlockhash    solana.Hash
	lastBlockID      solana.Hash
	haveLastBlockID  bool
}

func newStreamValidator(ref *state.RootedEventBatchRef) *streamValidator {
	return &streamValidator{ref: ref}
}

func (v *streamValidator) accept(event Event) error {
	if event.SchemaVersion != SchemaVersion {
		return fmt.Errorf("event schema %d is unsupported", event.SchemaVersion)
	}
	if !v.started || event.Cursor.Slot != v.slot {
		if v.started && !v.slotClosed {
			return fmt.Errorf("slot %d has no terminal root marker", v.slot)
		}
		if !v.started && event.Cursor.Slot != v.ref.FromSlot {
			return fmt.Errorf("first event slot %d does not match reference %d", event.Cursor.Slot, v.ref.FromSlot)
		}
		if v.started && event.Cursor.Slot <= v.slot {
			return fmt.Errorf("event slot %d does not follow %d", event.Cursor.Slot, v.slot)
		}
		v.started = true
		v.slot = event.Cursor.Slot
		v.nextOrdinal = 0
		v.accountCount = 0
		v.transactionCount = 0
		v.accountsStarted = false
		v.lastAccountKey = solana.PublicKey{}
		v.haveAccountKey = false
		v.slotClosed = false
	}
	if event.Cursor.Ordinal != v.nextOrdinal {
		return fmt.Errorf("slot %d ordinal %d does not follow %d", v.slot, event.Cursor.Ordinal, v.nextOrdinal)
	}
	v.nextOrdinal++
	switch event.Kind {
	case TransactionExecuted:
		if v.slotClosed || v.accountsStarted || event.Transaction == nil || event.Account != nil || event.Root != nil {
			return fmt.Errorf("slot %d has malformed transaction event", v.slot)
		}
		if event.Transaction.Index != v.transactionCount {
			return fmt.Errorf("slot %d transaction index %d does not follow %d", v.slot, event.Transaction.Index, v.transactionCount)
		}
		observation := TransactionObservation{
			Index: event.Transaction.Index, Signature: event.Transaction.Signature,
			Transaction: event.Transaction.Transaction, MessageHash: event.Transaction.MessageHash,
			AccountKeys: event.Transaction.AccountKeys,
			Succeeded:   event.Transaction.Succeeded, Failure: event.Transaction.Failure,
			ComputeUnits: event.Transaction.ComputeUnits, Logs: event.Transaction.Logs,
			LogsTruncated: event.Transaction.LogsTruncated,
			Inner:         event.Transaction.Inner, ReturnData: event.Transaction.ReturnData,
		}
		if err := validateTransaction(v.slot, v.transactionCount, observation); err != nil {
			return err
		}
		v.transactionCount++
	case AccountUpdated:
		if v.slotClosed || event.Account == nil || event.Transaction != nil || event.Root != nil {
			return fmt.Errorf("slot %d has malformed account event", v.slot)
		}
		key, err := solana.PublicKeyFromBase58(event.Account.Pubkey)
		if err != nil {
			return fmt.Errorf("slot %d has invalid account pubkey: %w", v.slot, err)
		}
		if _, err := solana.PublicKeyFromBase58(event.Account.Owner); err != nil {
			return fmt.Errorf("slot %d has invalid account owner: %w", v.slot, err)
		}
		if len(event.Account.Data) > maxAccountDataBytes {
			return fmt.Errorf("slot %d account %s data exceeds %d bytes", v.slot, event.Account.Pubkey, maxAccountDataBytes)
		}
		if event.Account.Tombstone != (event.Account.Lamports == 0) {
			return fmt.Errorf("slot %d account %s tombstone does not match its lamport balance", v.slot, event.Account.Pubkey)
		}
		if v.haveAccountKey && bytes.Compare(v.lastAccountKey[:], key[:]) >= 0 {
			return fmt.Errorf("slot %d account %s is not in strictly ascending order", v.slot, event.Account.Pubkey)
		}
		v.lastAccountKey = key
		v.haveAccountKey = true
		v.accountsStarted = true
		v.accountCount++
	case SlotRooted:
		if v.slotClosed || event.Root == nil || event.Transaction != nil || event.Account != nil {
			return fmt.Errorf("slot %d has malformed root event", v.slot)
		}
		if event.Root.AccountCount != v.accountCount {
			return fmt.Errorf("slot %d root count %d does not match %d account events", v.slot, event.Root.AccountCount, v.accountCount)
		}
		if event.Root.TransactionCount != v.transactionCount {
			return fmt.Errorf("slot %d root count %d does not match %d transaction events", v.slot, event.Root.TransactionCount, v.transactionCount)
		}
		bankhash, err := solana.HashFromBase58(event.Root.Bankhash)
		if err != nil {
			return fmt.Errorf("slot %d has invalid bankhash: %w", v.slot, err)
		}
		if bankhash == (solana.Hash{}) {
			return fmt.Errorf("slot %d has an empty bankhash", v.slot)
		}
		blockhash, err := solana.HashFromBase58(event.Root.Blockhash)
		if err != nil || blockhash == (solana.Hash{}) {
			return fmt.Errorf("slot %d has invalid or empty blockhash", v.slot)
		}
		parentBlockhash, err := solana.HashFromBase58(event.Root.ParentBlockhash)
		if err != nil || (v.slot > 0 && parentBlockhash == (solana.Hash{})) {
			return fmt.Errorf("slot %d has invalid or empty parent blockhash", v.slot)
		}
		hasBlockID := event.Root.BlockID != "" || event.Root.ParentBlockID != ""
		var blockID, parentBlockID solana.Hash
		if hasBlockID {
			if event.Root.BlockID == "" || event.Root.ParentBlockID == "" {
				return fmt.Errorf("slot %d has incomplete Alpenglow block identity", v.slot)
			}
			blockID, err = solana.HashFromBase58(event.Root.BlockID)
			if err != nil || blockID == (solana.Hash{}) {
				return fmt.Errorf("slot %d has invalid or empty Alpenglow block ID", v.slot)
			}
			parentBlockID, err = solana.HashFromBase58(event.Root.ParentBlockID)
			if err != nil || parentBlockID == (solana.Hash{}) {
				return fmt.Errorf("slot %d has invalid or empty Alpenglow parent block ID", v.slot)
			}
		}
		switch event.Root.FinalitySource {
		case FinalityAlpenglowCertificate, FinalityAlpenglowDelegated:
			if !hasBlockID {
				return fmt.Errorf("slot %d Alpenglow finality has no block identity", v.slot)
			}
		case FinalityRPCFinalized:
			if hasBlockID {
				return fmt.Errorf("slot %d RPC finality unexpectedly carries Alpenglow block identity", v.slot)
			}
		default:
			return fmt.Errorf("slot %d has invalid finality source %q", v.slot, event.Root.FinalitySource)
		}
		if v.haveRoot {
			if event.Root.ParentSlot != v.lastRoot {
				return fmt.Errorf("slot %d parent %d does not match previous root %d", v.slot, event.Root.ParentSlot, v.lastRoot)
			}
			if parentBlockhash != v.lastBlockhash {
				return fmt.Errorf("slot %d parent blockhash does not match previous root", v.slot)
			}
			if hasBlockID != v.haveLastBlockID || (hasBlockID && parentBlockID != v.lastBlockID) {
				return fmt.Errorf("slot %d parent block ID does not match previous root", v.slot)
			}
		} else if v.slot > 0 && event.Root.ParentSlot >= v.slot {
			return fmt.Errorf("slot %d has invalid parent %d", v.slot, event.Root.ParentSlot)
		}
		v.slotClosed = true
		v.lastRoot = v.slot
		v.haveRoot = true
		v.lastBlockhash = blockhash
		v.lastBlockID = blockID
		v.haveLastBlockID = hasBlockID
	default:
		return fmt.Errorf("slot %d has unknown event kind %q", v.slot, event.Kind)
	}
	return nil
}

func (v *streamValidator) finish() error {
	if !v.started || !v.slotClosed {
		return errors.New("event stream has no terminal root marker")
	}
	if v.lastRoot != v.ref.ThroughSlot {
		return fmt.Errorf("last rooted slot %d does not match reference %d", v.lastRoot, v.ref.ThroughSlot)
	}
	return nil
}

func sidecarDir(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("rooted events: AccountsDB root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve AccountsDB root for rooted events: %w", err)
	}
	return filepath.Join(filepath.Clean(abs), SidecarDirectory), nil
}

func rootedEventDirectoryExists(dir string) (bool, error) {
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat rooted-event directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("rooted-event path is not a real non-symlink directory")
	}
	return true, nil
}

func sidecarBasename(from, through uint64, digest string) string {
	return sidecarPrefix + strconv.FormatUint(from, 10) + "-" + strconv.FormatUint(through, 10) + "-" + digest + sidecarSuffix
}

func parseSidecarBasename(name string) (*state.RootedEventBatchRef, bool) {
	if !strings.HasPrefix(name, sidecarPrefix) || !strings.HasSuffix(name, sidecarSuffix) {
		return nil, false
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(name, sidecarPrefix), sidecarSuffix), "-")
	if len(parts) != 3 {
		return nil, false
	}
	from, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return nil, false
	}
	through, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return nil, false
	}
	ref := &state.RootedEventBatchRef{
		Version:     SidecarVersion,
		FromSlot:    from,
		ThroughSlot: through,
		File:        name,
		SHA256:      parts[2],
		Size:        1,
	}
	if err := ValidateSidecarRef(ref); err != nil {
		return nil, false
	}
	return ref, true
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
