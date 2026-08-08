package controlaudit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/internal/safefile"
)

var (
	ErrConflictingDuplicate = errors.New("event ID already has different content")
	ErrChainMismatch        = errors.New("event does not extend the audit chain")
	ErrSequenceMismatch     = errors.New("event sequence does not extend the audit chain")
	ErrStoreLimit           = errors.New("audit store reached its configured limit")
	ErrStoreUncertain       = errors.New("audit store durability is uncertain")
	ErrStoreClosed          = errors.New("audit store is closed")
	ErrInvalidPhase         = errors.New("invalid audit phase transition")
)

const (
	DefaultMaxStoreBytes   = uint64(64 << 20)
	DefaultMaxStoreRecords = uint64(65_536)

	// The change token is the live integrity boundary. This periodic full scan
	// is a bounded second check against filesystem or kernel defects without
	// turning every receipt and summary into an O(store-size) operation.
	storeFullVerifyInterval = 10 * time.Minute
)

// StoreLimits bounds the durable receiver state. Zero fields select the safe
// production defaults.
type StoreLimits struct {
	MaxBytes   uint64
	MaxRecords uint64
}

type storedRecord struct {
	receipt Receipt
}

type actionState struct {
	binding        ApprovalBinding
	phase          Phase
	firstTimestamp time.Time
	lastTimestamp  time.Time
	lastEvent      Event
}

type storeChangeToken struct {
	modifiedNanoseconds int64
	changedSeconds      int64
	changedNanoseconds  int64
}

// Summary identifies a completely verified prefix of the receiver store.
type Summary struct {
	Records      uint64
	LastSequence uint64
	TipHash      string
	Bytes        uint64
}

// Store is one append-only, hash-chained control-audit file.
type Store struct {
	mu        sync.Mutex
	path      string
	file      *os.File
	verifier  ApprovalVerifier
	records   map[string]storedRecord
	actions   map[string]actionState
	active    string
	holdUntil time.Time
	summary   Summary
	limits    StoreLimits
	content   hash.Hash
	change    storeChangeToken
	verified  time.Time
	write     func([]byte) (int, error)
	now       func() time.Time
	poisoned  bool
	closed    bool
}

// OpenStore opens or creates one private store and verifies its entire
// existing chain before accepting new records.
func OpenStore(path string, verifier ApprovalVerifier) (*Store, error) {
	store, _, _, err := openStoreWithSnapshotLimits(
		context.Background(),
		path,
		verifier,
		StoreLimits{},
		false,
	)
	return store, err
}

// OpenStoreWithLimits opens a store with explicit resource bounds. Zero limit
// fields select the production defaults.
func OpenStoreWithLimits(
	path string,
	verifier ApprovalVerifier,
	limits StoreLimits,
) (*Store, error) {
	store, _, _, err := openStoreWithSnapshotLimits(
		context.Background(),
		path,
		verifier,
		limits,
		false,
	)
	return store, err
}

// OpenStoreWithSnapshot opens or creates one private store, verifies its
// existing chain once, and returns that verified prefix while retaining the
// exclusive append lock.
func OpenStoreWithSnapshot(
	ctx context.Context,
	path string,
	verifier ApprovalVerifier,
) (*Store, []Event, Summary, error) {
	return openStoreWithSnapshotLimits(
		ctx,
		path,
		verifier,
		StoreLimits{},
		true,
	)
}

func openStoreWithSnapshotLimits(
	ctx context.Context,
	path string,
	verifier ApprovalVerifier,
	limits StoreLimits,
	collect bool,
) (*Store, []Event, Summary, error) {
	if ctx == nil {
		return nil, nil, Summary{}, errors.New("open audit store: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, Summary{}, err
	}
	if verifier == nil {
		return nil, nil, Summary{}, ErrApprovalVerifierRequired
	}
	limits = normalizedStoreLimits(limits)
	file, created, err := openStoreFile(path, true)
	if err != nil {
		return nil, nil, Summary{}, err
	}
	if created {
		if err := syncParent(filepath.Dir(path)); err != nil {
			_ = file.Close()
			return nil, nil, Summary{}, fmt.Errorf("create audit store: %w", err)
		}
	}
	initialInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, Summary{}, ErrStoreUncertain
	}
	initialChange, err := fileChangeToken(initialInfo)
	if err != nil {
		_ = file.Close()
		return nil, nil, Summary{}, ErrStoreUncertain
	}

	events, records, actions, active, holdUntil, summary, err := readAndVerify(
		ctx,
		file,
		verifier,
		collect,
		limits,
	)
	if err != nil {
		_ = file.Close()
		return nil, nil, Summary{}, err
	}
	content, err := storeContentHash(file, summary.Bytes)
	if err != nil {
		_ = file.Close()
		return nil, nil, Summary{}, ErrStoreUncertain
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		_ = file.Close()
		return nil, nil, Summary{}, errors.New("position audit store for append")
	}
	store := &Store{
		path:      path,
		file:      file,
		verifier:  verifier,
		records:   records,
		actions:   actions,
		active:    active,
		holdUntil: holdUntil,
		summary:   summary,
		limits:    limits,
		content:   content,
		change:    initialChange,
		verified:  time.Now(),
		now:       time.Now,
	}
	currentChange, err := store.inspectOpenFile(summary.Bytes)
	if err != nil || currentChange != initialChange {
		_ = file.Close()
		return nil, nil, Summary{}, ErrStoreUncertain
	}
	return store, events, summary, nil
}

// Append verifies, appends and fsyncs an event. Duplicate is true when an
// identical event was already durable, which makes a lost acknowledgement
// safe to retry.
func (store *Store) Append(ctx context.Context, event Event) (receipt Receipt, duplicate bool, err error) {
	if ctx == nil {
		return Receipt{}, false, errors.New("append audit event: nil context")
	}
	encoded, err := MarshalEvent(event)
	if err != nil {
		return Receipt{}, false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return Receipt{}, false, ErrStoreClosed
	}
	if store.poisoned {
		return Receipt{}, false, ErrStoreUncertain
	}
	if err := store.verifyOpenFile(); err != nil {
		store.poisoned = true
		return Receipt{}, false, ErrStoreUncertain
	}
	if err := ctx.Err(); err != nil {
		return Receipt{}, false, err
	}
	if existing, ok := store.records[event.ID]; ok {
		if existing.receipt.EventHash != event.EventHash {
			return Receipt{}, false, ErrConflictingDuplicate
		}
		if err := store.verifyOpenFile(); err != nil {
			store.poisoned = true
			return Receipt{}, false, ErrStoreUncertain
		}
		return existing.receipt, true, nil
	}
	binding, err := store.verifier.VerifyApproval(ctx, event)
	if err != nil {
		return Receipt{}, false, fmt.Errorf("%w: %w", ErrApprovalRejected, err)
	}
	if err := validateApprovalBinding(event, binding); err != nil {
		return Receipt{}, false, err
	}
	if current, exists := store.actions[event.ActionID]; exists {
		if current.lastEvent.ID == "" {
			return Receipt{}, false, fmt.Errorf(
				"%w: previous action checkpoint is unavailable",
				ErrInvalidPhase,
			)
		}
		if err := store.verifier.VerifyStateTransition(
			ctx,
			cloneEvent(current.lastEvent),
			cloneEvent(event),
		); err != nil {
			return Receipt{}, false, fmt.Errorf(
				"%w: state checkpoint transition was rejected",
				ErrInvalidPhase,
			)
		}
	}
	if err := ctx.Err(); err != nil {
		return Receipt{}, false, err
	}
	if event.Sequence != store.summary.LastSequence+1 {
		return Receipt{}, false, ErrSequenceMismatch
	}
	if event.PreviousHash != store.summary.TipHash {
		return Receipt{}, false, ErrChainMismatch
	}
	if _, exists := store.actions[event.ActionID]; !exists {
		if !binding.CanStartAction {
			return Receipt{}, false, fmt.Errorf(
				"%w: approver key is not active",
				ErrApprovalRejected,
			)
		}
		if !store.holdUntil.IsZero() &&
			store.now != nil &&
			store.now().UTC().Before(store.holdUntil) {
			return Receipt{}, false, fmt.Errorf(
				"%w: previous approval generation has not expired",
				ErrInvalidPhase,
			)
		}
	}
	recordBytes := uint64(len(encoded) + 1)
	if store.summary.Records >= store.limits.MaxRecords ||
		recordBytes > store.limits.MaxBytes-store.summary.Bytes {
		return Receipt{}, false, ErrStoreLimit
	}
	nextActive, nextHoldUntil, err := validateActionTransition(
		store.actions,
		store.active,
		store.holdUntil,
		event,
		binding,
	)
	if err != nil {
		return Receipt{}, false, err
	}
	record := append(encoded, '\n')
	write := store.file.Write
	if store.write != nil {
		write = store.write
	}
	if written, err := write(record); err != nil || written != len(record) {
		if written > 0 && written < len(record) {
			_ = store.file.Truncate(int64(store.summary.Bytes))
			_ = store.file.Sync()
		}
		store.poisoned = true
		return Receipt{}, false, ErrStoreUncertain
	}
	if err := store.file.Sync(); err != nil {
		store.poisoned = true
		return Receipt{}, false, ErrStoreUncertain
	}
	expectedBytes := store.summary.Bytes + recordBytes
	nextChange, err := store.inspectOpenFile(expectedBytes)
	if err != nil || nextChange == store.change {
		store.poisoned = true
		return Receipt{}, false, ErrStoreUncertain
	}
	writtenRecord := make([]byte, len(record))
	read, readErr := store.file.ReadAt(writtenRecord, int64(store.summary.Bytes))
	if readErr != nil || read != len(record) || !bytes.Equal(writtenRecord, record) {
		store.poisoned = true
		return Receipt{}, false, ErrStoreUncertain
	}
	confirmedChange, err := store.inspectOpenFile(expectedBytes)
	if err != nil || confirmedChange != nextChange {
		store.poisoned = true
		return Receipt{}, false, ErrStoreUncertain
	}
	_, _ = store.content.Write(record)
	contentDigest, err := digestStoreFile(store.file, expectedBytes)
	if err != nil || !bytes.Equal(contentDigest[:], store.content.Sum(nil)) {
		store.poisoned = true
		return Receipt{}, false, ErrStoreUncertain
	}
	contentChange, err := store.inspectOpenFile(expectedBytes)
	if err != nil || contentChange != confirmedChange {
		store.poisoned = true
		return Receipt{}, false, ErrStoreUncertain
	}
	store.verified = time.Now()
	store.change = confirmedChange

	receipt = receiptFor(event)
	store.records[event.ID] = storedRecord{receipt: receipt}
	store.summary.Records++
	store.summary.LastSequence = event.Sequence
	store.summary.TipHash = event.EventHash
	store.summary.Bytes += recordBytes
	state := store.actions[event.ActionID]
	if state.firstTimestamp.IsZero() {
		state.firstTimestamp = mustParseTimestamp(event.Timestamp)
	}
	state.binding = binding
	state.phase = event.Phase
	state.lastTimestamp = mustParseTimestamp(event.Timestamp)
	if terminalPhase(event.Phase) {
		state.lastEvent = Event{}
	} else {
		state.lastEvent = cloneEvent(event)
	}
	store.actions[event.ActionID] = state
	store.active = nextActive
	store.holdUntil = nextHoldUntil
	return receipt, false, nil
}

// Summary verifies the live file before returning its durable chain prefix.
func (store *Store) Summary() (Summary, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return Summary{}, ErrStoreClosed
	}
	if store.poisoned {
		return Summary{}, ErrStoreUncertain
	}
	if err := store.verifyOpenFile(); err != nil {
		store.poisoned = true
		return Summary{}, ErrStoreUncertain
	}
	return store.summary, nil
}

// Close flushes and closes the store.
func (store *Store) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	var integrityErr error
	if store.poisoned {
		integrityErr = ErrStoreUncertain
	} else if store.verifyOpenFile() != nil {
		store.poisoned = true
		integrityErr = ErrStoreUncertain
	}
	return errors.Join(integrityErr, store.file.Sync(), store.file.Close())
}

func (store *Store) verifyOpenFile() error {
	change, err := store.inspectOpenFile(store.summary.Bytes)
	if err != nil || change != store.change {
		return errors.New("audit store changed")
	}
	elapsed := time.Since(store.verified)
	if !store.verified.IsZero() && elapsed >= 0 && elapsed < storeFullVerifyInterval {
		return nil
	}
	return store.verifyContentAtChange(change)
}

func (store *Store) verifyContentAtChange(change storeChangeToken) error {
	digest, err := digestStoreFile(store.file, store.summary.Bytes)
	if err != nil || !bytes.Equal(digest[:], store.content.Sum(nil)) {
		return errors.New("audit store content changed")
	}
	confirmedChange, err := store.inspectOpenFile(store.summary.Bytes)
	if err != nil || confirmedChange != change {
		return errors.New("audit store changed during verification")
	}
	store.verified = time.Now()
	return nil
}

func (store *Store) inspectOpenFile(expectedBytes uint64) (storeChangeToken, error) {
	if err := safefile.ValidateNoSymlinkAncestors(store.path); err != nil {
		return storeChangeToken{}, errors.New("audit store directory changed")
	}
	parentInfo, err := os.Lstat(filepath.Dir(store.path))
	if err != nil || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() ||
		!safefile.OwnerTrusted(parentInfo) || parentInfo.Mode().Perm()&0o077 != 0 {
		return storeChangeToken{}, errors.New("audit store directory changed")
	}
	pathInfo, err := os.Lstat(store.path)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() ||
		!safefile.OwnerTrusted(pathInfo) || pathInfo.Mode().Perm()&0o077 != 0 {
		return storeChangeToken{}, errors.New("audit store path changed")
	}
	fileInfo, err := store.file.Stat()
	if err != nil || !os.SameFile(pathInfo, fileInfo) {
		return storeChangeToken{}, errors.New("audit store object changed")
	}
	if fileInfo.Size() < 0 || uint64(fileInfo.Size()) != expectedBytes {
		return storeChangeToken{}, errors.New("audit store size changed")
	}
	change, err := fileChangeToken(fileInfo)
	if err != nil {
		return storeChangeToken{}, errors.New("audit store change token is unavailable")
	}
	return change, nil
}

// fileChangeToken extracts mtime and ctime without depending on one Unix
// Stat_t layout. Secure stores are Unix-only, but the field is named Ctim on
// Linux and Ctimespec on Darwin and BSD. ctime catches a same-length rewrite
// even when a writer restores mtime afterwards.
func fileChangeToken(info os.FileInfo) (storeChangeToken, error) {
	if info == nil {
		return storeChangeToken{}, errors.New("missing audit store file information")
	}
	system := reflect.ValueOf(info.Sys())
	if !system.IsValid() {
		return storeChangeToken{}, errors.New("missing audit store system information")
	}
	if system.Kind() == reflect.Pointer {
		if system.IsNil() {
			return storeChangeToken{}, errors.New("missing audit store system information")
		}
		system = system.Elem()
	}
	if system.Kind() != reflect.Struct {
		return storeChangeToken{}, errors.New("invalid audit store system information")
	}
	for _, name := range []string{"Ctim", "Ctimespec"} {
		change := system.FieldByName(name)
		if !change.IsValid() || change.Kind() != reflect.Struct {
			continue
		}
		seconds, secondsOK := reflectedInteger(change.FieldByName("Sec"))
		nanoseconds, nanosecondsOK := reflectedInteger(change.FieldByName("Nsec"))
		if secondsOK && nanosecondsOK {
			return storeChangeToken{
				modifiedNanoseconds: info.ModTime().UnixNano(),
				changedSeconds:      seconds,
				changedNanoseconds:  nanoseconds,
			}, nil
		}
	}
	seconds, secondsOK := reflectedInteger(system.FieldByName("Ctime"))
	nanoseconds, nanosecondsOK := reflectedInteger(system.FieldByName("Ctimensec"))
	if secondsOK && nanosecondsOK {
		return storeChangeToken{
			modifiedNanoseconds: info.ModTime().UnixNano(),
			changedSeconds:      seconds,
			changedNanoseconds:  nanoseconds,
		}, nil
	}
	return storeChangeToken{}, errors.New("audit store ctime is unavailable")
}

func reflectedInteger(value reflect.Value) (int64, bool) {
	if !value.IsValid() {
		return 0, false
	}
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return int64(value.Uint()), true
	default:
		return 0, false
	}
}

func storeContentHash(file *os.File, size uint64) (hash.Hash, error) {
	content := sha256.New()
	if err := hashStoreFile(content, file, size); err != nil {
		return nil, err
	}
	return content, nil
}

func digestStoreFile(file *os.File, size uint64) ([sha256.Size]byte, error) {
	content := sha256.New()
	if err := hashStoreFile(content, file, size); err != nil {
		return [sha256.Size]byte{}, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], content.Sum(nil))
	return digest, nil
}

func hashStoreFile(content hash.Hash, file *os.File, size uint64) error {
	if size > uint64(^uint64(0)>>1) {
		return errors.New("audit store size is invalid")
	}
	info, err := file.Stat()
	if err != nil || info.Size() < 0 || uint64(info.Size()) != size {
		return errors.New("audit store size changed")
	}
	read, err := io.Copy(
		content,
		io.NewSectionReader(file, 0, int64(size)),
	)
	if err != nil || read != int64(size) {
		return errors.New("audit store content is unreadable")
	}
	return nil
}

// Restore reads and independently verifies an existing off-host chain.
func Restore(ctx context.Context, path string, verifier ApprovalVerifier) ([]Event, Summary, error) {
	if ctx == nil {
		return nil, Summary{}, errors.New("restore audit store: nil context")
	}
	if verifier == nil {
		return nil, Summary{}, ErrApprovalVerifierRequired
	}
	file, _, err := openStoreFile(path, false)
	if err != nil {
		return nil, Summary{}, err
	}
	defer file.Close()
	events, _, _, _, _, summary, err := readAndVerify(
		ctx,
		file,
		verifier,
		true,
		normalizedStoreLimits(StoreLimits{}),
	)
	if err != nil {
		return nil, Summary{}, err
	}
	return events, summary, nil
}

func readAndVerify(
	ctx context.Context,
	file *os.File,
	verifier ApprovalVerifier,
	collect bool,
	limits StoreLimits,
) ([]Event, map[string]storedRecord, map[string]actionState, string, time.Time, Summary, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, nil, nil, "", time.Time{}, Summary{}, errors.New("inspect audit store")
	}
	if info.Size() < 0 || uint64(info.Size()) > limits.MaxBytes {
		return nil, nil, nil, "", time.Time{}, Summary{}, ErrStoreLimit
	}
	if info.Size() > 0 {
		var last [1]byte
		if _, err := file.ReadAt(last[:], info.Size()-1); err != nil || last[0] != '\n' {
			return nil, nil, nil, "", time.Time{}, Summary{}, errors.New("audit store ends with a partial record")
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, nil, nil, "", time.Time{}, Summary{}, errors.New("read audit store")
	}

	records := make(map[string]storedRecord)
	actions := make(map[string]actionState)
	var active string
	var holdUntil time.Time
	summary := Summary{Bytes: uint64(info.Size())}
	var events []Event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), MaxEventBytes+1)
	for scanner.Scan() {
		if summary.Records >= limits.MaxRecords {
			return nil, nil, nil, "", time.Time{}, Summary{}, ErrStoreLimit
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, "", time.Time{}, Summary{}, err
		}
		event, err := ParseEvent(scanner.Bytes())
		if err != nil {
			return nil, nil, nil, "", time.Time{}, Summary{}, fmt.Errorf("verify audit store record %d: %w", summary.Records+1, err)
		}
		if event.Sequence != summary.LastSequence+1 {
			return nil, nil, nil, "", time.Time{}, Summary{}, fmt.Errorf("verify audit store record %d: %w", summary.Records+1, ErrSequenceMismatch)
		}
		if event.PreviousHash != summary.TipHash {
			return nil, nil, nil, "", time.Time{}, Summary{}, fmt.Errorf("verify audit store record %d: %w", summary.Records+1, ErrChainMismatch)
		}
		if _, exists := records[event.ID]; exists {
			return nil, nil, nil, "", time.Time{}, Summary{}, fmt.Errorf("verify audit store record %d: duplicate event ID", summary.Records+1)
		}
		binding, err := verifier.VerifyApproval(ctx, event)
		if err != nil {
			return nil, nil, nil, "", time.Time{}, Summary{}, fmt.Errorf("verify audit store record %d: %w: %w", summary.Records+1, ErrApprovalRejected, err)
		}
		if err := validateApprovalBinding(event, binding); err != nil {
			return nil, nil, nil, "", time.Time{}, Summary{}, fmt.Errorf("verify audit store record %d: %w", summary.Records+1, err)
		}
		if current, exists := actions[event.ActionID]; exists {
			if current.lastEvent.ID == "" {
				return nil, nil, nil, "", time.Time{}, Summary{}, fmt.Errorf(
					"verify audit store record %d: %w: previous action checkpoint is unavailable",
					summary.Records+1,
					ErrInvalidPhase,
				)
			}
			if err := verifier.VerifyStateTransition(
				ctx,
				cloneEvent(current.lastEvent),
				cloneEvent(event),
			); err != nil {
				return nil, nil, nil, "", time.Time{}, Summary{}, fmt.Errorf(
					"verify audit store record %d: %w: state checkpoint transition was rejected",
					summary.Records+1,
					ErrInvalidPhase,
				)
			}
		}
		nextActive, nextHoldUntil, err := validateActionTransition(
			actions,
			active,
			holdUntil,
			event,
			binding,
		)
		if err != nil {
			return nil, nil, nil, "", time.Time{}, Summary{}, fmt.Errorf("verify audit store record %d: %w", summary.Records+1, err)
		}
		receipt := receiptFor(event)
		records[event.ID] = storedRecord{receipt: receipt}
		summary.Records++
		summary.LastSequence = event.Sequence
		summary.TipHash = event.EventHash
		state := actions[event.ActionID]
		if state.firstTimestamp.IsZero() {
			state.firstTimestamp = mustParseTimestamp(event.Timestamp)
		}
		state.binding = binding
		state.phase = event.Phase
		state.lastTimestamp = mustParseTimestamp(event.Timestamp)
		if terminalPhase(event.Phase) {
			state.lastEvent = Event{}
		} else {
			state.lastEvent = cloneEvent(event)
		}
		actions[event.ActionID] = state
		active = nextActive
		holdUntil = nextHoldUntil
		if collect {
			events = append(events, event)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, nil, "", time.Time{}, Summary{}, errors.New("read audit store")
	}
	return events, records, actions, active, holdUntil, summary, nil
}

func normalizedStoreLimits(limits StoreLimits) StoreLimits {
	if limits.MaxBytes == 0 {
		limits.MaxBytes = DefaultMaxStoreBytes
	}
	if limits.MaxRecords == 0 {
		limits.MaxRecords = DefaultMaxStoreRecords
	}
	return limits
}

func validateApprovalBinding(event Event, binding ApprovalBinding) error {
	evidenceHash := sha256.Sum256(event.ApprovalEvidence)
	if binding.SessionID != event.SessionID ||
		binding.TargetID != event.TargetID ||
		binding.ActionID != event.ActionID ||
		binding.Action != event.Action ||
		binding.Unit != event.Unit ||
		binding.Scope != event.Scope ||
		binding.BeforeHash != event.BeforeHash ||
		binding.ApproverKeyID != event.ApproverKeyID ||
		binding.EvidenceSHA256 != fmt.Sprintf("%x", evidenceHash) {
		return fmt.Errorf("%w: approval binding does not match event", ErrApprovalRejected)
	}
	if binding.ExpiresAtUnix <= binding.IssuedAtUnix ||
		binding.ExpiresAtUnix-binding.IssuedAtUnix <= 0 ||
		binding.ExpiresAtUnix-binding.IssuedAtUnix > int64(MaxApprovalWindow/time.Second) {
		return fmt.Errorf("%w: approval validity window is invalid", ErrApprovalRejected)
	}
	return nil
}

func validateActionTransition(
	actions map[string]actionState,
	active string,
	holdUntil time.Time,
	event Event,
	binding ApprovalBinding,
) (string, time.Time, error) {
	timestamp := mustParseTimestamp(event.Timestamp)
	current, exists := actions[event.ActionID]
	if !exists {
		if active != "" {
			return active, holdUntil, fmt.Errorf("%w: another action remains nonterminal", ErrInvalidPhase)
		}
		if event.Phase != PhasePrepared {
			return active, holdUntil, fmt.Errorf("%w: action does not start prepared", ErrInvalidPhase)
		}
		if timestamp.Before(holdUntil) {
			return active, holdUntil, fmt.Errorf("%w: previous approval generation has not expired", ErrInvalidPhase)
		}
		issued := time.Unix(binding.IssuedAtUnix, 0)
		expires := time.Unix(binding.ExpiresAtUnix, 0)
		if timestamp.Before(issued) || !timestamp.Before(expires) {
			return active, holdUntil, fmt.Errorf("%w: initial event is outside approval validity", ErrApprovalRejected)
		}
		return event.ActionID, holdUntil, nil
	}
	if current.binding != binding {
		return active, holdUntil, fmt.Errorf("%w: approval changed during the action", ErrApprovalRejected)
	}
	if timestamp.Before(current.lastTimestamp) {
		return active, holdUntil, fmt.Errorf("%w: action timestamp regressed", ErrInvalidPhase)
	}
	if !phaseTransitionAllowed(current.phase, event.Phase) {
		return active, holdUntil, fmt.Errorf("%w: %s to %s", ErrInvalidPhase, current.phase, event.Phase)
	}
	if terminalPhase(event.Phase) {
		barrier := current.firstTimestamp.Add(MaxApprovalWindow)
		expires := time.Unix(binding.ExpiresAtUnix, 0)
		if expires.After(barrier) {
			barrier = expires
		}
		return "", barrier, nil
	}
	return event.ActionID, holdUntil, nil
}

func phaseTransitionAllowed(from, to Phase) bool {
	switch from {
	case PhasePrepared:
		return to == PhaseDispatchStarted || to == PhaseFailed
	case PhaseDispatchStarted:
		return to == PhaseDispatched || to == PhaseFailed || to == PhaseOutcomeUnknown
	case PhaseDispatched:
		return to == PhaseVerifying || to == PhaseOutcomeUnknown
	case PhaseVerifying:
		return to == PhaseSucceeded || to == PhaseOutcomeUnknown
	default:
		return false
	}
}

func terminalPhase(phase Phase) bool {
	return phase == PhaseSucceeded || phase == PhaseFailed || phase == PhaseOutcomeUnknown
}

func mustParseTimestamp(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

func cloneEvent(event Event) Event {
	cloned := event
	cloned.ApprovalEvidence = bytes.Clone(event.ApprovalEvidence)
	cloned.StateCheckpoint = bytes.Clone(event.StateCheckpoint)
	return cloned
}

func openStoreFile(path string, create bool) (*os.File, bool, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, false, errors.New("audit store path must be a clean absolute path")
	}
	if err := safefile.ValidateNoSymlinkAncestors(path); err != nil {
		return nil, false, errors.New("audit store path must not traverse a symlink")
	}
	parent := filepath.Dir(path)
	parentInfo, err := os.Lstat(parent)
	if err != nil || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return nil, false, errors.New("audit store directory is unavailable")
	}
	if !safefile.OwnerTrusted(parentInfo) || parentInfo.Mode().Perm()&0o077 != 0 {
		return nil, false, errors.New("audit store directory is not private")
	}

	if create {
		file, err := createStoreObject(path)
		if err == nil {
			return file, true, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, false, errors.New("create audit store")
		}
	}

	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, false, errors.New("audit store is unavailable")
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("audit store must not be a symlink")
	}
	if !pathInfo.Mode().IsRegular() || !safefile.OwnerTrusted(pathInfo) || pathInfo.Mode().Perm()&0o077 != 0 {
		return nil, false, errors.New("audit store is not a private regular file")
	}
	file, err := openExistingStoreObject(path, create)
	if err != nil {
		return nil, false, errors.New("open audit store")
	}
	fileInfo, err := file.Stat()
	if err != nil || !os.SameFile(pathInfo, fileInfo) {
		_ = file.Close()
		return nil, false, errors.New("audit store changed while opening")
	}
	return file, false, nil
}

func syncParent(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
