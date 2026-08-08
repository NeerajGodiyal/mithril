package mcp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/internal/controlaudit"
	"github.com/Overclock-Validator/mithril/internal/safefile"
)

const (
	controlAuditClientConfigVersion = uint16(1)
	maxControlAuditClientConfig     = 16 << 10
	maxControlAuditTLSMaterial      = 1 << 20
	maxControlAuditPending          = 8
	maxControlAuditLocalBytes       = 64 << 20
	maxControlAuditLocalRecords     = 65_536
	controlAuditStoreName           = "control-audit.jsonl"
)

var (
	errControlAuditPending        = errors.New("control audit delivery remains pending")
	errControlAuditBound          = errors.New("control audit store reached its configured bound")
	errControlAuditRemoteRejected = errors.New("off-host control audit rejected the event")
	errControlAuditHistoricalKey  = errors.New(
		"historical approver public key is not configured; retain every key referenced by the audit store",
	)
)

type controlAuditClientFile struct {
	Version               uint16 `json:"version"`
	Endpoint              string `json:"endpoint"`
	ServerName            string `json:"server_name"`
	ServerSPKIPin         string `json:"server_spki_pin"`
	ClientCertificatePath string `json:"client_certificate_path"`
	ClientPrivateKeyPath  string `json:"client_private_key_path"`
	ServerCAPath          string `json:"server_ca_path"`
}

// loadControlAuditClient reads only trusted, bounded configuration and TLS
// material. Private-key source bytes are erased immediately after parsing.
func loadControlAuditClient(path string) (*controlaudit.Client, error) {
	raw, err := safefile.ReadTrustedRegular(path, safefile.ReadOptions{
		MaxBytes:               maxControlAuditClientConfig,
		ForbiddenPerm:          0o022,
		RejectAncestorSymlinks: true,
	})
	if err != nil {
		return nil, errors.New("control audit client configuration is unavailable")
	}
	defer clear(raw)

	config, err := parseControlAuditClientFile(raw)
	if err != nil {
		return nil, err
	}
	pin, err := controlaudit.ParsePublicKeyPin(config.ServerSPKIPin)
	if err != nil {
		return nil, errors.New("control audit server identity is invalid")
	}

	certificatePEM, err := readControlAuditTLSFile(config.ClientCertificatePath, false)
	if err != nil {
		return nil, errors.New("control audit client certificate is unavailable")
	}
	defer clear(certificatePEM)
	privateKeyPEM, err := readControlAuditTLSFile(config.ClientPrivateKeyPath, true)
	if err != nil {
		return nil, errors.New("control audit client private key is unavailable")
	}
	defer clear(privateKeyPEM)
	certificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		return nil, errors.New("control audit client certificate or private key is invalid")
	}

	caPEM, err := readControlAuditTLSFile(config.ServerCAPath, false)
	if err != nil {
		return nil, errors.New("control audit server CA is unavailable")
	}
	defer clear(caPEM)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("control audit server CA is invalid")
	}

	client, err := controlaudit.NewClient(controlaudit.ClientConfig{
		Endpoint:     config.Endpoint,
		Certificate:  certificate,
		ServerRoots:  roots,
		ServerName:   config.ServerName,
		ServerKeyPin: pin,
	})
	if err != nil {
		return nil, errors.New("control audit client configuration is invalid")
	}
	return client, nil
}

func parseControlAuditClientFile(raw []byte) (controlAuditClientFile, error) {
	if len(raw) == 0 || len(raw) > maxControlAuditClientConfig {
		return controlAuditClientFile{}, errors.New("control audit client configuration has an invalid size")
	}
	if err := rejectDuplicateControlAuditClientFields(raw); err != nil {
		return controlAuditClientFile{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var config controlAuditClientFile
	if err := decoder.Decode(&config); err != nil {
		return controlAuditClientFile{}, errors.New("control audit client configuration is malformed")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return controlAuditClientFile{}, errors.New("control audit client configuration has trailing data")
	}
	if config.Version != controlAuditClientConfigVersion {
		return controlAuditClientFile{}, errors.New("control audit client configuration version is unsupported")
	}
	if !validControlAuditEndpoint(config.Endpoint) ||
		!validControlAuditServerName(config.ServerName) ||
		config.ServerSPKIPin == "" {
		return controlAuditClientFile{}, errors.New("control audit client configuration fields are invalid")
	}
	paths := []string{
		config.ClientCertificatePath,
		config.ClientPrivateKeyPath,
		config.ServerCAPath,
	}
	for _, path := range paths {
		if path == "" || len(path) > 4096 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return controlAuditClientFile{}, errors.New("control audit TLS path is invalid")
		}
	}
	if config.ClientCertificatePath == config.ClientPrivateKeyPath ||
		config.ClientPrivateKeyPath == config.ServerCAPath {
		return controlAuditClientFile{}, errors.New("control audit private key must use a distinct file")
	}
	return config, nil
}

func rejectDuplicateControlAuditClientFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("control audit client configuration is malformed")
	}
	seen := make(map[string]struct{}, 7)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return errors.New("control audit client configuration is malformed")
		}
		if _, duplicate := seen[name]; duplicate {
			return errors.New("control audit client configuration contains a duplicate field")
		}
		seen[name] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return errors.New("control audit client configuration is malformed")
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return errors.New("control audit client configuration is malformed")
	}
	return nil
}

func validControlAuditEndpoint(value string) bool {
	if len(value) == 0 || len(value) > 2048 {
		return false
	}
	endpoint, err := url.Parse(value)
	return err == nil &&
		endpoint.Scheme == "https" &&
		endpoint.Host != "" &&
		endpoint.User == nil &&
		(endpoint.Path == "" || endpoint.Path == "/") &&
		endpoint.RawPath == "" &&
		endpoint.RawQuery == "" &&
		!endpoint.ForceQuery &&
		endpoint.Fragment == ""
}

func validControlAuditServerName(value string) bool {
	if len(value) == 0 || len(value) > 253 || strings.TrimSpace(value) != value {
		return false
	}
	if net.ParseIP(value) != nil {
		return true
	}
	value = strings.TrimSuffix(value, ".")
	if value == "" {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 ||
			label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') &&
				(character < 'A' || character > 'Z') &&
				(character < '0' || character > '9') &&
				character != '-' {
				return false
			}
		}
	}
	return true
}

func readControlAuditTLSFile(path string, private bool) ([]byte, error) {
	forbidden := os.FileMode(0o022)
	if private {
		forbidden = 0o077
	}
	return safefile.ReadTrustedRegular(path, safefile.ReadOptions{
		MaxBytes:               maxControlAuditTLSMaterial,
		ForbiddenPerm:          forbidden,
		RejectAncestorSymlinks: true,
	})
}

func validateControlApprovalEvidenceShape(evidence ControlApprovalEvidence) error {
	if evidence.Version != approvalVersion ||
		evidence.Domain != serviceApprovalAuditDomain ||
		!approvalIDPattern.MatchString(evidence.ActionID) ||
		!approvalIDPattern.MatchString(evidence.ApproverKeyID) ||
		evidence.AuthorizationClaimsSHA256 == ([sha256.Size]byte{}) ||
		evidence.NonceSHA256 == ([sha256.Size]byte{}) ||
		evidence.IssuedAtUnix <= 0 ||
		evidence.ExpiresAtUnix <= evidence.IssuedAtUnix ||
		len(evidence.ClaimsCBOR) == 0 ||
		len(evidence.ClaimsCBOR) > maxApprovalTokenBytes ||
		len(evidence.Proof) != ed25519.SignatureSize ||
		evidence.EvidenceSHA256 != approvalEvidenceHash(
			evidence.Domain,
			evidence.ClaimsCBOR,
			evidence.Proof,
		) {
		return errors.New("control approval evidence is invalid")
	}
	return nil
}

// marshalControlApprovalEvidence returns the only audit-event representation
// accepted for persistable, non-authorizing approval evidence.
func marshalControlApprovalEvidence(evidence ControlApprovalEvidence) ([]byte, error) {
	if err := validateControlApprovalEvidenceShape(evidence); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(evidence)
	if err != nil || len(raw) > controlaudit.MaxApprovalEvidenceBytes {
		return nil, errors.New("control approval evidence cannot be encoded")
	}
	return raw, nil
}

func parseControlApprovalEvidence(raw []byte) (ControlApprovalEvidence, error) {
	if len(raw) == 0 || len(raw) > controlaudit.MaxApprovalEvidenceBytes {
		return ControlApprovalEvidence{}, errors.New("control approval evidence has an invalid size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var evidence ControlApprovalEvidence
	if err := decoder.Decode(&evidence); err != nil {
		return ControlApprovalEvidence{}, errors.New("control approval evidence is malformed")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ControlApprovalEvidence{}, errors.New("control approval evidence has trailing data")
	}
	canonical, err := marshalControlApprovalEvidence(evidence)
	if err != nil || !bytes.Equal(raw, canonical) {
		return ControlApprovalEvidence{}, errors.New("control approval evidence is not canonical")
	}
	return evidence, nil
}

type controlAuditApprovalVerifier struct {
	publicKeys map[string]ed25519.PublicKey
	activeKeys map[string]struct{}
}

func newControlAuditApprovalVerifier(
	publicKeys map[string]ed25519.PublicKey,
) controlaudit.ApprovalVerifier {
	return newControlAuditApprovalVerifierWithActive(publicKeys, publicKeys)
}

func newControlAuditApprovalVerifierWithActive(
	publicKeys map[string]ed25519.PublicKey,
	activeKeys map[string]ed25519.PublicKey,
) controlaudit.ApprovalVerifier {
	keys := make(map[string]ed25519.PublicKey, len(publicKeys))
	for id, publicKey := range publicKeys {
		keys[id] = ed25519.PublicKey(bytes.Clone(publicKey))
	}
	active := make(map[string]struct{}, len(activeKeys))
	for id, publicKey := range activeKeys {
		known, ok := keys[id]
		if ok && bytes.Equal(known, publicKey) {
			active[id] = struct{}{}
		}
	}
	return controlAuditApprovalVerifier{
		publicKeys: keys,
		activeKeys: active,
	}
}

// NewControlAuditApprovalVerifier loads active authorization keys and the
// retained key history used to verify the complete audit chain.
func NewControlAuditApprovalVerifier(
	approverKeysDir string,
	approverHistoryKeysDir string,
) (controlaudit.ApprovalVerifier, error) {
	active, err := loadApproverPublicKeys(approverKeysDir)
	if err != nil {
		return nil, errors.New("active control approver keys are unavailable")
	}
	if approverHistoryKeysDir == "" {
		approverHistoryKeysDir = approverKeysDir
	}
	history, err := loadApproverPublicKeys(approverHistoryKeysDir)
	if err != nil {
		return nil, errors.New("historical control approver keys are unavailable")
	}
	verificationKeys, err := mergeApproverPublicKeys(active, history)
	if err != nil {
		return nil, errors.New("control approver key sets conflict")
	}
	return newControlAuditApprovalVerifierWithActive(
		verificationKeys,
		active,
	), nil
}

// ControlRestoreConfig identifies a copied off-host audit chain and the one
// operator target it is allowed to restore.
type ControlRestoreConfig struct {
	ControlStateDir        string
	ApproverKeysDir        string
	ApproverHistoryKeysDir string
	AuditClientConfigPath  string
	TargetID               string
	SystemdUnit            string
	SystemdScope           string
}

// ControlRestoreResult reports only the verified chain identity and whether a
// missing operation file was created.
type ControlRestoreResult struct {
	Records       uint64 `json:"records"`
	TipHash       string `json:"tip_hash,omitempty"`
	ActionID      string `json:"action_id,omitempty"`
	Phase         string `json:"phase,omitempty"`
	StateRestored bool   `json:"state_restored"`
}

// RestoreControlState reconstructs operation.json from a copied audit file
// after the live pinned receiver proves that the copy is the complete chain.
func RestoreControlState(
	ctx context.Context,
	config ControlRestoreConfig,
) (ControlRestoreResult, error) {
	if ctx == nil {
		return ControlRestoreResult{}, errors.New("restore control state: nil context")
	}
	if err := ctx.Err(); err != nil {
		return ControlRestoreResult{}, err
	}
	if !approvalIDPattern.MatchString(config.TargetID) {
		return ControlRestoreResult{}, errors.New("restore target ID is invalid")
	}
	if err := ValidateSystemdServiceUnit(config.SystemdUnit); err != nil {
		return ControlRestoreResult{}, errors.New("restore systemd unit is invalid")
	}
	if config.SystemdScope != "system" && config.SystemdScope != "user" {
		return ControlRestoreResult{}, errors.New("restore systemd scope is invalid")
	}
	activeKeys, err := loadApproverPublicKeys(config.ApproverKeysDir)
	if err != nil {
		return ControlRestoreResult{}, errors.New("restore active approver keys are unavailable")
	}
	historyDir := config.ApproverHistoryKeysDir
	if historyDir == "" {
		historyDir = config.ApproverKeysDir
	}
	historicalKeys := activeKeys
	if historyDir != config.ApproverKeysDir {
		historicalKeys, err = loadApproverPublicKeys(historyDir)
		if err != nil {
			return ControlRestoreResult{}, errors.New("restore historical approver keys are unavailable")
		}
	}
	keys, err := mergeApproverPublicKeys(activeKeys, historicalKeys)
	if err != nil {
		return ControlRestoreResult{}, errors.New("restore approver key sets conflict")
	}
	verifier := newControlAuditApprovalVerifierWithActive(keys, activeKeys)
	auditPath := filepath.Join(config.ControlStateDir, controlAuditStoreName)
	events, localSummary, err := controlaudit.Restore(ctx, auditPath, verifier)
	if err != nil {
		return ControlRestoreResult{}, errors.New("copied control audit is invalid")
	}
	client, err := loadControlAuditClient(config.AuditClientConfigPath)
	if err != nil {
		return ControlRestoreResult{}, errors.New("restore audit client is unavailable")
	}
	defer client.Close()
	remoteSummary, err := client.Summary(ctx)
	if err != nil {
		return ControlRestoreResult{}, errors.New("off-host control audit summary is unavailable")
	}
	if err := validateControlRestoreSummary(localSummary, remoteSummary); err != nil {
		return ControlRestoreResult{}, err
	}
	return restoreControlStateSnapshot(
		ctx,
		config,
		keys,
		events,
		localSummary,
	)
}

func validateControlRestoreSummary(
	local controlaudit.Summary,
	remote controlaudit.Summary,
) error {
	if remote != local {
		return errors.New("copied control audit is not the complete receiver chain")
	}
	return nil
}

func restoreControlStateSnapshot(
	ctx context.Context,
	config ControlRestoreConfig,
	keys map[string]ed25519.PublicKey,
	events []controlaudit.Event,
	summary controlaudit.Summary,
) (ControlRestoreResult, error) {
	result := ControlRestoreResult{
		Records: summary.Records,
		TipHash: summary.TipHash,
	}
	if len(events) == 0 {
		return result, nil
	}
	for _, event := range events {
		if event.TargetID != config.TargetID ||
			event.Unit != config.SystemdUnit ||
			event.Scope != config.SystemdScope {
			return ControlRestoreResult{}, errors.New("copied control audit does not match the configured target")
		}
	}
	operation, err := operationFromControlAuditEvent(events[len(events)-1])
	if err != nil {
		return ControlRestoreResult{}, errors.New("latest control audit checkpoint is invalid")
	}
	if err := verifyServiceOperationApproval(operation, keys); err != nil {
		return ControlRestoreResult{}, errors.New("latest control audit approval is invalid")
	}
	state, err := newControlStateStore(
		filepath.Join(config.ControlStateDir, "operation.json"),
	)
	if err != nil {
		return ControlRestoreResult{}, errors.New("restore control state directory is unavailable")
	}
	restored := false
	if err := state.withTransaction(ctx, func(transaction *controlStateTransaction) error {
		current := transaction.operation()
		if current == nil {
			if err := transaction.restore(operation); err != nil {
				return err
			}
			restored = true
			return nil
		}
		currentRaw, err := marshalControlState(*current)
		if err != nil {
			return err
		}
		restoredRaw, err := marshalControlState(operation)
		if err != nil {
			return err
		}
		if !bytes.Equal(currentRaw, restoredRaw) {
			return errors.New("a different control operation state already exists")
		}
		return nil
	}); err != nil {
		return ControlRestoreResult{}, errors.New("control operation state could not be restored")
	}
	result.ActionID = operation.ID
	result.Phase = string(operation.Phase)
	result.StateRestored = restored
	return result, nil
}

func (verifier controlAuditApprovalVerifier) VerifyApproval(
	ctx context.Context,
	event controlaudit.Event,
) (controlaudit.ApprovalBinding, error) {
	if ctx == nil {
		return controlaudit.ApprovalBinding{}, errors.New("control audit approval context is nil")
	}
	if err := ctx.Err(); err != nil {
		return controlaudit.ApprovalBinding{}, err
	}
	evidence, err := parseControlApprovalEvidence(event.ApprovalEvidence)
	if err != nil {
		return controlaudit.ApprovalBinding{}, errors.New("control audit approval evidence was rejected")
	}
	if _, ok := verifier.publicKeys[evidence.ApproverKeyID]; !ok {
		return controlaudit.ApprovalBinding{}, errControlAuditHistoricalKey
	}
	binding, err := VerifyControlApprovalEvidence(
		evidence,
		verifier.publicKeys,
	)
	if err != nil {
		return controlaudit.ApprovalBinding{}, errors.New("control audit approval evidence was rejected")
	}
	operation, err := parseControlStateCheckpoint(event.StateCheckpoint)
	if err != nil || !operationMatchesEvent(operation, event) {
		return controlaudit.ApprovalBinding{}, errors.New("control audit state checkpoint was rejected")
	}
	rawHash := sha256.Sum256(event.ApprovalEvidence)
	return controlaudit.ApprovalBinding{
		SessionID:      binding.ServerSession,
		TargetID:       binding.TargetID,
		ActionID:       binding.ActionID,
		Action:         controlaudit.Action(binding.Action),
		Unit:           binding.Unit,
		Scope:          binding.Scope,
		BeforeHash:     binding.BeforeHash,
		ApproverKeyID:  binding.ApproverKeyID,
		IssuedAtUnix:   binding.IssuedAtUnix,
		ExpiresAtUnix:  binding.ExpiresAtUnix,
		EvidenceSHA256: hex.EncodeToString(rawHash[:]),
		CanStartAction: verifier.keyIsActive(binding.ApproverKeyID),
	}, nil
}

func (verifier controlAuditApprovalVerifier) keyIsActive(keyID string) bool {
	_, ok := verifier.activeKeys[keyID]
	return ok
}

func (verifier controlAuditApprovalVerifier) VerifyStateTransition(
	ctx context.Context,
	previous controlaudit.Event,
	next controlaudit.Event,
) error {
	if ctx == nil {
		return errors.New("control audit transition context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := operationFromControlAuditEvent(previous)
	if err != nil {
		return errors.New("previous control audit checkpoint was rejected")
	}
	after, err := operationFromControlAuditEvent(next)
	if err != nil {
		return errors.New("next control audit checkpoint was rejected")
	}
	if !sameOperationIdentity(before, after) ||
		!validOperationDeadlineTransition(before, after) {
		return errors.New("control audit checkpoint changed operation identity")
	}
	return nil
}

type controlAuditAppender interface {
	Append(context.Context, controlaudit.Event) (controlaudit.Receipt, error)
	Summary(context.Context) (controlaudit.Summary, error)
}

type controlAuditEventFields struct {
	Timestamp  time.Time
	AfterHash  string
	Outcome    string
	ReasonCode string
}

type controlAuditTrail struct {
	mu          sync.Mutex
	path        string
	store       *controlaudit.Store
	remote      controlAuditAppender
	closeRemote func()
	pending     []controlaudit.Event
	last        controlaudit.Event
	hasLast     bool
	closed      bool
}

// openControlAuditTrail verifies the bounded local chain under its exclusive
// append lock, then brings the receiver to the same prefix.
func openControlAuditTrail(
	ctx context.Context,
	controlStateDir string,
	auditClientConfigPath string,
	verifier controlaudit.ApprovalVerifier,
) (*controlAuditTrail, error) {
	client, err := loadControlAuditClient(auditClientConfigPath)
	if err != nil {
		return nil, err
	}
	trail, err := openControlAuditTrailWithRemote(
		ctx,
		filepath.Join(controlStateDir, controlAuditStoreName),
		verifier,
		client,
		client.Close,
	)
	if err != nil {
		client.Close()
		return nil, err
	}
	return trail, nil
}

func openControlAuditTrailWithRemote(
	ctx context.Context,
	path string,
	verifier controlaudit.ApprovalVerifier,
	remote controlAuditAppender,
	closeRemote func(),
) (*controlAuditTrail, error) {
	if ctx == nil {
		return nil, errors.New("open control audit trail: nil context")
	}
	if verifier == nil || remote == nil {
		return nil, errors.New("control audit trail dependencies are incomplete")
	}
	if err := checkControlAuditStoreBound(path, true); err != nil {
		return nil, err
	}

	store, events, summary, err := controlaudit.OpenStoreWithSnapshot(ctx, path, verifier)
	if err != nil {
		if errors.Is(err, controlaudit.ErrApprovalRejected) &&
			errors.Is(err, errControlAuditHistoricalKey) {
			return nil, errors.New(
				"local control audit store needs a historical approver public key; retain every key referenced by the audit store",
			)
		}
		return nil, errors.New("local control audit store is unavailable")
	}
	if summary.Bytes > maxControlAuditLocalBytes ||
		summary.Records > maxControlAuditLocalRecords ||
		uint64(len(events)) != summary.Records {
		_ = store.Close()
		return nil, errControlAuditBound
	}
	remoteSummary, err := remote.Summary(ctx)
	if err != nil {
		_ = store.Close()
		return nil, errors.New("off-host control audit summary is unavailable")
	}
	suffix, err := controlAuditMissingSuffix(events, summary, remoteSummary)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	for _, event := range suffix {
		receipt, err := remote.Append(ctx, cloneControlAuditEvent(event))
		if errors.Is(err, controlaudit.ErrPermanentRejection) ||
			(err == nil && !controlAuditReceiptMatches(receipt, event)) {
			_ = store.Close()
			return nil, errControlAuditRemoteRejected
		}
		if err != nil {
			_ = store.Close()
			return nil, errors.New("off-host control audit is not synchronized")
		}
	}

	var last controlaudit.Event
	hasLast := len(events) != 0
	if hasLast {
		last = cloneControlAuditEvent(events[len(events)-1])
	}
	return &controlAuditTrail{
		path:        path,
		store:       store,
		remote:      remote,
		closeRemote: closeRemote,
		last:        last,
		hasLast:     hasLast,
	}, nil
}

func controlAuditMissingSuffix(
	events []controlaudit.Event,
	local controlaudit.Summary,
	remote controlaudit.Summary,
) ([]controlaudit.Event, error) {
	if remote.Records > local.Records {
		return nil, errors.New("off-host control audit is ahead of the local chain")
	}
	if remote.Records == 0 {
		return events, nil
	}
	index := remote.Records - 1
	if index >= uint64(len(events)) {
		return nil, errors.New("local control audit snapshot is inconsistent")
	}
	prefix := events[index]
	if remote.LastSequence != prefix.Sequence ||
		remote.TipHash != prefix.EventHash {
		return nil, errors.New("off-host control audit prefix does not match the local chain")
	}
	var prefixBytes uint64
	for _, event := range events[:remote.Records] {
		encoded, err := controlaudit.MarshalEvent(event)
		if err != nil {
			return nil, errors.New("local control audit snapshot is inconsistent")
		}
		prefixBytes += uint64(len(encoded) + 1)
	}
	if remote.Bytes != prefixBytes {
		return nil, errors.New("off-host control audit prefix size does not match the local chain")
	}
	return events[remote.Records:], nil
}

func checkControlAuditStoreBound(path string, absentOK bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && absentOK {
		return nil
	}
	if err != nil {
		return errors.New("local control audit path is unavailable")
	}
	if info.Size() > maxControlAuditLocalBytes {
		return errControlAuditBound
	}
	return nil
}

func checkControlAuditAppendCapacity(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("local control audit path is unavailable")
	}
	if info.Size() > maxControlAuditLocalBytes-(controlaudit.MaxEventBytes+1) {
		return errControlAuditBound
	}
	return nil
}

func (trail *controlAuditTrail) appendAndAcknowledge(
	ctx context.Context,
	operation serviceOperation,
	fields controlAuditEventFields,
) (controlaudit.Event, error) {
	if ctx == nil {
		return controlaudit.Event{}, errors.New("append control audit event: nil context")
	}
	trail.mu.Lock()
	defer trail.mu.Unlock()
	if trail.closed {
		return controlaudit.Event{}, errors.New("control audit trail is closed")
	}
	if err := trail.syncPendingLocked(ctx); err != nil {
		return controlaudit.Event{}, err
	}
	event, err := trail.appendLocalLocked(ctx, operation, fields)
	if err != nil {
		return controlaudit.Event{}, err
	}
	trail.pending = append(trail.pending, cloneControlAuditEvent(event))
	if err := trail.syncPendingLocked(ctx); err != nil {
		return cloneControlAuditEvent(event), err
	}
	return cloneControlAuditEvent(event), nil
}

// appendLocalAndQueue records post-dispatch evidence even when the receiver is
// unavailable. Delivery remains ordered, bounded, and must drain before any
// later pre-dispatch append can succeed.
func (trail *controlAuditTrail) appendLocalAndQueue(
	ctx context.Context,
	operation serviceOperation,
	fields controlAuditEventFields,
) (controlaudit.Event, error) {
	if ctx == nil {
		return controlaudit.Event{}, errors.New("append control audit event: nil context")
	}
	trail.mu.Lock()
	defer trail.mu.Unlock()
	if trail.closed {
		return controlaudit.Event{}, errors.New("control audit trail is closed")
	}
	// Make one ordered delivery attempt, but do not let a receiver outage erase
	// later knowledge about an action that may already have executed.
	_ = trail.syncPendingLocked(ctx)
	if len(trail.pending) >= maxControlAuditPending {
		return controlaudit.Event{}, errControlAuditPending
	}
	event, err := trail.appendLocalLocked(ctx, operation, fields)
	if err != nil {
		return controlaudit.Event{}, err
	}
	trail.pending = append(trail.pending, cloneControlAuditEvent(event))
	if err := trail.syncPendingLocked(ctx); err != nil {
		return cloneControlAuditEvent(event), err
	}
	return cloneControlAuditEvent(event), nil
}

func (trail *controlAuditTrail) appendLocalLocked(
	ctx context.Context,
	operation serviceOperation,
	fields controlAuditEventFields,
) (controlaudit.Event, error) {
	if len(trail.pending) >= maxControlAuditPending {
		return controlaudit.Event{}, errControlAuditPending
	}
	summary, err := trail.store.Summary()
	if err != nil {
		return controlaudit.Event{}, errors.New("local control audit store is unavailable")
	}
	if summary.Records >= maxControlAuditLocalRecords {
		return controlaudit.Event{}, errControlAuditBound
	}
	if err := checkControlAuditAppendCapacity(trail.path); err != nil {
		return controlaudit.Event{}, err
	}

	event, err := controlAuditEvent(operation, fields, summary)
	if err != nil {
		return controlaudit.Event{}, err
	}
	if _, duplicate, err := trail.store.Append(ctx, event); err != nil {
		return controlaudit.Event{}, errors.New("local control audit append failed")
	} else if duplicate {
		return controlaudit.Event{}, errors.New("local control audit event unexpectedly already exists")
	}
	trail.last = cloneControlAuditEvent(event)
	trail.hasLast = true
	return event, nil
}

func controlAuditEvent(
	operation serviceOperation,
	fields controlAuditEventFields,
	summary controlaudit.Summary,
) (controlaudit.Event, error) {
	if err := operation.validate(); err != nil {
		return controlaudit.Event{}, errors.New("control operation is invalid for audit")
	}
	evidence, err := marshalControlApprovalEvidence(operation.Approval)
	if err != nil {
		return controlaudit.Event{}, err
	}
	checkpoint, err := marshalControlStateCheckpoint(operation)
	if err != nil {
		return controlaudit.Event{}, err
	}
	timestamp := time.Unix(operation.UpdatedAtUnix, 0).UTC()
	if !fields.Timestamp.IsZero() && !fields.Timestamp.UTC().Equal(timestamp) {
		return controlaudit.Event{}, errors.New("control audit timestamp does not match operation state")
	}
	if fields.AfterHash != operation.AfterHash ||
		fields.Outcome != operation.Outcome ||
		fields.ReasonCode != operation.ReasonCode {
		return controlaudit.Event{}, errors.New("control audit result does not match operation state")
	}
	sequence := summary.LastSequence + 1
	id := controlAuditEventID(operation.ID, sequence, operation.Phase, timestamp)
	event := controlaudit.Event{
		Version:                 controlaudit.ProtocolVersion,
		Sequence:                sequence,
		ID:                      id,
		Timestamp:               controlaudit.CanonicalTimestamp(timestamp),
		SessionID:               operation.ServerSession,
		TargetID:                operation.TargetID,
		ActionID:                operation.ID,
		Phase:                   controlaudit.Phase(operation.Phase),
		Action:                  controlaudit.Action(operation.Action),
		Unit:                    operation.Unit,
		Scope:                   operation.Scope,
		ApproverKeyID:           operation.Approval.ApproverKeyID,
		ApprovalEvidence:        evidence,
		StateCheckpoint:         checkpoint,
		BeforeHash:              operation.BeforeHash,
		AfterHash:               fields.AfterHash,
		Outcome:                 fields.Outcome,
		ReasonCode:              fields.ReasonCode,
		DispatchMayHaveOccurred: operation.DispatchMayHaveOccurred,
		DispatchAccepted:        operation.DispatchAccepted,
		PreviousHash:            summary.TipHash,
	}
	return controlaudit.Seal(event)
}

func controlAuditEventID(
	actionID string,
	sequence uint64,
	phase operationPhase,
	timestamp time.Time,
) string {
	digest := sha256.New()
	_, _ = fmt.Fprintf(
		digest,
		"%s\x00%d\x00%s\x00%s",
		actionID,
		sequence,
		phase,
		controlaudit.CanonicalTimestamp(timestamp),
	)
	return "evt-" + base64.RawURLEncoding.EncodeToString(digest.Sum(nil)[:24])
}

func controlAuditReceiptMatches(
	receipt controlaudit.Receipt,
	event controlaudit.Event,
) bool {
	return receipt.Version == controlaudit.ProtocolVersion &&
		receipt.EventID == event.ID &&
		receipt.EventHash == event.EventHash &&
		receipt.Sequence == event.Sequence
}

func operationMatchesEvent(
	operation serviceOperation,
	event controlaudit.Event,
) bool {
	if operation.validate() != nil || event.Validate() != nil {
		return false
	}
	evidence, err := marshalControlApprovalEvidence(operation.Approval)
	if err != nil {
		return false
	}
	checkpoint, err := marshalControlStateCheckpoint(operation)
	if err != nil {
		return false
	}
	timestamp := time.Unix(operation.UpdatedAtUnix, 0).UTC()
	return event.Version == controlaudit.ProtocolVersion &&
		event.ID == controlAuditEventID(
			operation.ID,
			event.Sequence,
			operation.Phase,
			timestamp,
		) &&
		event.Timestamp == controlaudit.CanonicalTimestamp(timestamp) &&
		event.SessionID == operation.ServerSession &&
		event.TargetID == operation.TargetID &&
		event.ActionID == operation.ID &&
		event.Phase == controlaudit.Phase(operation.Phase) &&
		event.Action == controlaudit.Action(operation.Action) &&
		event.Unit == operation.Unit &&
		event.Scope == operation.Scope &&
		event.ApproverKeyID == operation.Approval.ApproverKeyID &&
		bytes.Equal(event.ApprovalEvidence, evidence) &&
		bytes.Equal(event.StateCheckpoint, checkpoint) &&
		event.BeforeHash == operation.BeforeHash &&
		event.AfterHash == operation.AfterHash &&
		event.Outcome == operation.Outcome &&
		event.ReasonCode == operation.ReasonCode &&
		event.DispatchMayHaveOccurred == operation.DispatchMayHaveOccurred &&
		event.DispatchAccepted == operation.DispatchAccepted
}

func cloneControlAuditEvent(event controlaudit.Event) controlaudit.Event {
	copy := event
	copy.ApprovalEvidence = bytes.Clone(event.ApprovalEvidence)
	copy.StateCheckpoint = bytes.Clone(event.StateCheckpoint)
	return copy
}

func operationFromControlAuditEvent(
	event controlaudit.Event,
) (serviceOperation, error) {
	if err := event.Validate(); err != nil {
		return serviceOperation{}, errors.New("control audit event is invalid")
	}
	operation, err := parseControlStateCheckpoint(event.StateCheckpoint)
	if err != nil || !operationMatchesEvent(operation, event) {
		return serviceOperation{}, errors.New("control audit state checkpoint does not match its event")
	}
	return operation, nil
}

func (trail *controlAuditTrail) syncPending(ctx context.Context) error {
	if ctx == nil {
		return errors.New("sync control audit events: nil context")
	}
	trail.mu.Lock()
	defer trail.mu.Unlock()
	if trail.closed {
		return errors.New("control audit trail is closed")
	}
	return trail.syncPendingLocked(ctx)
}

func (trail *controlAuditTrail) syncPendingLocked(ctx context.Context) error {
	summary, err := trail.store.Summary()
	if err != nil {
		return errors.New("local control audit store is unavailable")
	}
	if trail.hasLast {
		if summary.Records == 0 ||
			summary.LastSequence != trail.last.Sequence ||
			summary.TipHash != trail.last.EventHash {
			return errors.New("local control audit store does not match its verified tip")
		}
	} else if summary.Records != 0 ||
		summary.LastSequence != 0 ||
		summary.TipHash != "" {
		return errors.New("local control audit store does not match its verified tip")
	}
	for len(trail.pending) != 0 {
		event := trail.pending[0]
		receipt, err := trail.remote.Append(ctx, cloneControlAuditEvent(event))
		if errors.Is(err, controlaudit.ErrPermanentRejection) ||
			(err == nil && !controlAuditReceiptMatches(receipt, event)) {
			return errControlAuditRemoteRejected
		}
		if err != nil {
			return errControlAuditPending
		}
		clear(trail.pending[0].ApprovalEvidence)
		copy(trail.pending, trail.pending[1:])
		trail.pending[len(trail.pending)-1] = controlaudit.Event{}
		trail.pending = trail.pending[:len(trail.pending)-1]
	}
	return nil
}

func (trail *controlAuditTrail) lastEvent() (controlaudit.Event, bool) {
	trail.mu.Lock()
	defer trail.mu.Unlock()
	return cloneControlAuditEvent(trail.last), trail.hasLast
}

func (trail *controlAuditTrail) close() error {
	trail.mu.Lock()
	defer trail.mu.Unlock()
	if trail.closed {
		return nil
	}
	trail.closed = true
	if trail.closeRemote != nil {
		trail.closeRemote()
	}
	for index := range trail.pending {
		clear(trail.pending[index].ApprovalEvidence)
	}
	clear(trail.pending)
	trail.pending = nil
	return trail.store.Close()
}
