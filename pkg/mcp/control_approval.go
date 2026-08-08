package mcp

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/Overclock-Validator/mithril/internal/safefile"
	"github.com/fxamacker/cbor/v2"
)

const (
	approvalVersion             = uint16(1)
	approvalTokenPrefix         = "v1"
	serviceApprovalDomain       = "mithril.service.lifecycle.v1"
	serviceApprovalAuditDomain  = "mithril.service.lifecycle.audit.v1"
	approvalNonceBytes          = 24
	maxApprovalChallengeBytes   = 8 * 1024
	maxApprovalTokenBytes       = 8 * 1024
	maxApproverPrivateKeyBytes  = ed25519.SeedSize
	maxApproverPublicKeyFiles   = 32
	approverPublicKeyFileSuffix = ".pub"
)

var approvalIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,126}$`)

var (
	approvalEncMode = mustApprovalEncMode()
	approvalDecMode = mustApprovalDecMode()
)

func mustApprovalEncMode() cbor.EncMode {
	mode, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	return mode
}

func mustApprovalDecMode() cbor.DecMode {
	mode, err := (cbor.DecOptions{
		DupMapKey:         cbor.DupMapKeyEnforcedAPF,
		MaxNestedLevels:   4,
		MaxArrayElements:  16,
		MaxMapPairs:       16,
		IndefLength:       cbor.IndefLengthForbidden,
		TagsMd:            cbor.TagsForbidden,
		ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
		UTF8:              cbor.UTF8RejectInvalid,
		NaN:               cbor.NaNDecodeForbidden,
		Inf:               cbor.InfDecodeForbidden,
	}).DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}

type approvalClaims struct {
	Version       uint16        `cbor:"1,keyasint"`
	Domain        string        `cbor:"2,keyasint"`
	ServerSession string        `cbor:"3,keyasint"`
	TargetID      string        `cbor:"4,keyasint"`
	ActionID      string        `cbor:"5,keyasint"`
	Action        serviceAction `cbor:"6,keyasint"`
	Unit          string        `cbor:"7,keyasint"`
	Scope         string        `cbor:"8,keyasint"`
	BeforeHash    string        `cbor:"9,keyasint"`
	Nonce         [24]byte      `cbor:"10,keyasint"`
	IssuedAtUnix  int64         `cbor:"11,keyasint"`
	ExpiresAtUnix int64         `cbor:"12,keyasint"`
	ApproverKeyID string        `cbor:"13,keyasint"`
}

type approvalAuditClaims struct {
	Version                   uint16        `cbor:"1,keyasint"`
	Domain                    string        `cbor:"2,keyasint"`
	AuthorizationClaimsSHA256 [32]byte      `cbor:"3,keyasint"`
	ServerSession             string        `cbor:"4,keyasint"`
	TargetID                  string        `cbor:"5,keyasint"`
	ActionID                  string        `cbor:"6,keyasint"`
	Action                    serviceAction `cbor:"7,keyasint"`
	Unit                      string        `cbor:"8,keyasint"`
	Scope                     string        `cbor:"9,keyasint"`
	BeforeHash                string        `cbor:"10,keyasint"`
	NonceSHA256               [32]byte      `cbor:"11,keyasint"`
	IssuedAtUnix              int64         `cbor:"12,keyasint"`
	ExpiresAtUnix             int64         `cbor:"13,keyasint"`
	ApproverKeyID             string        `cbor:"14,keyasint"`
}

type approvalDisplayStatus struct {
	Unit                            string `cbor:"1,keyasint"`
	Scope                           string `cbor:"2,keyasint"`
	LoadState                       string `cbor:"3,keyasint"`
	ActiveState                     string `cbor:"4,keyasint"`
	SubState                        string `cbor:"5,keyasint"`
	Result                          string `cbor:"6,keyasint,omitempty"`
	NRestarts                       uint64 `cbor:"7,keyasint"`
	MainPID                         uint64 `cbor:"8,keyasint"`
	InvocationID                    string `cbor:"9,keyasint,omitempty"`
	ActiveEnterTimestampMonotonic   uint64 `cbor:"10,keyasint"`
	InactiveEnterTimestampMonotonic uint64 `cbor:"11,keyasint"`
	Job                             string `cbor:"12,keyasint,omitempty"`
}

type approvalChallenge struct {
	Version uint16                `cbor:"1,keyasint"`
	Claims  approvalClaims        `cbor:"2,keyasint"`
	Status  approvalDisplayStatus `cbor:"3,keyasint"`
}

// ServiceApprovalBundle is the exact pair produced by the interactive
// approver. The audit attestation is domain-separated and cannot authorize an
// action by itself.
type ServiceApprovalBundle struct {
	AuthorizationToken string `json:"authorization_token"`
	AuditAttestation   string `json:"audit_attestation"`
}

// ServiceApprovalSummary contains the fields an operator must review before
// the private key is used.
type ServiceApprovalSummary struct {
	Action        string
	Unit          string
	Scope         string
	TargetID      string
	ActionID      string
	ApproverKeyID string
	ExpiresAt     int64
	Status        ServiceApprovalStatus
	Consequence   string
}

// ServiceApprovalStatus is the bounded current state shown by the approver.
type ServiceApprovalStatus struct {
	LoadState   string
	ActiveState string
	SubState    string
	NRestarts   uint64
	MainPID     uint64
}

// ControlApprovalEvidence is safe to persist because its proof is valid only
// in the non-authorizing audit domain. It never contains the bearer token.
type ControlApprovalEvidence struct {
	Version                   uint16   `json:"version"`
	Domain                    string   `json:"domain"`
	AuthorizationClaimsSHA256 [32]byte `json:"authorization_claims_sha256"`
	ActionID                  string   `json:"action_id"`
	ApproverKeyID             string   `json:"approver_key_id"`
	NonceSHA256               [32]byte `json:"nonce_sha256"`
	IssuedAtUnix              int64    `json:"issued_at_unix"`
	ExpiresAtUnix             int64    `json:"expires_at_unix"`
	ClaimsCBOR                []byte   `json:"claims_cbor"`
	Proof                     []byte   `json:"proof"`
	EvidenceSHA256            [32]byte `json:"evidence_sha256"`
}

// ControlApprovalBinding contains every audit-attested field that must match a
// surrounding durable control event.
type ControlApprovalBinding struct {
	ServerSession             string
	TargetID                  string
	ActionID                  string
	Action                    string
	Unit                      string
	Scope                     string
	BeforeHash                string
	ApproverKeyID             string
	AuthorizationClaimsSHA256 [32]byte
	NonceSHA256               [32]byte
	IssuedAtUnix              int64
	ExpiresAtUnix             int64
}

type approvalAuthority struct {
	publicKeys    map[string]ed25519.PublicKey
	serverSession string
	targetID      string
}

// ValidateControlTargetID accepts one stable deployment identifier.
func ValidateControlTargetID(targetID string) error {
	if !approvalIDPattern.MatchString(targetID) {
		return errors.New("control target ID must contain 1-127 letters, digits, dots, underscores, colons, or hyphens")
	}
	return nil
}

func (a approvalAuthority) configured() bool {
	return len(a.publicKeys) > 0 && a.serverSession != "" && a.targetID != ""
}

func (a approvalAuthority) keyIDs() []string {
	ids := make([]string, 0, len(a.publicKeys))
	for id := range a.publicKeys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (a approvalAuthority) resolveKeyID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if len(a.publicKeys) != 1 {
			return "", errors.New("approver_key_id is required when more than one approver is configured")
		}
		for id := range a.publicKeys {
			return id, nil
		}
	}
	if _, ok := a.publicKeys[raw]; !ok {
		return "", errors.New("approver key is not configured")
	}
	return raw, nil
}

func newApprovalAuthority(cfg Config, random io.Reader, load func(string) (map[string]ed25519.PublicKey, error)) (approvalAuthority, error) {
	if err := ValidateControlTargetID(cfg.ControlTargetID); err != nil {
		return approvalAuthority{}, fmt.Errorf("operator %w", err)
	}
	keys, err := load(cfg.ApproverKeysDir)
	if err != nil {
		return approvalAuthority{}, err
	}
	session, err := randomApprovalID(random)
	if err != nil {
		return approvalAuthority{}, errors.New("failed to create operator server session")
	}
	return approvalAuthority{publicKeys: keys, serverSession: session, targetID: cfg.ControlTargetID}, nil
}

func randomApprovalID(random io.Reader) (string, error) {
	var value [approvalNonceBytes]byte
	if _, err := io.ReadFull(random, value[:]); err != nil {
		return "", err
	}
	return encodeApprovalID(value[:]), nil
}

func approverKeyID(publicKey ed25519.PublicKey) string {
	sum := sha256.Sum256(publicKey)
	return encodeApprovalID(sum[:16])
}

func encodeApprovalID(value []byte) string {
	encoded := base64.RawURLEncoding.EncodeToString(value)
	// Base64url may start with punctuation that the ID grammar rejects.
	if len(encoded) > 0 && (encoded[0] == '-' || encoded[0] == '_') {
		return "x" + encoded
	}
	return encoded
}

// ApproverKeyID returns the stable identifier used in approval challenges.
func ApproverKeyID(publicKey ed25519.PublicKey) (string, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return "", errors.New("approver public key is invalid")
	}
	return approverKeyID(publicKey), nil
}

func readApproverPrivateKey(path string) (ed25519.PrivateKey, error) {
	data, err := safefile.ReadTrustedRegular(path, safefile.ReadOptions{
		MaxBytes:               maxApproverPrivateKeyBytes,
		ForbiddenPerm:          0o077,
		RejectAncestorSymlinks: true,
	})
	if err != nil {
		return nil, fmt.Errorf("MCP approver private key: %w", err)
	}
	if len(data) != ed25519.SeedSize {
		clear(data)
		return nil, fmt.Errorf("MCP approver private key must contain exactly %d bytes", ed25519.SeedSize)
	}
	privateKey := ed25519.NewKeyFromSeed(data)
	clear(data)
	return privateKey, nil
}

// LoadApproverPrivateKey is used by the interactive CLI. The MCP server never
// calls it and never receives private key bytes.
func LoadApproverPrivateKey(path string) (ed25519.PrivateKey, error) {
	return readApproverPrivateKey(path)
}

func loadApproverPublicKeys(path string) (map[string]ed25519.PublicKey, error) {
	return loadApproverPublicKeysOwnedBy(path, 0)
}

// loadApproverPublicKeysOwnedBy lets tests create fixtures without root. The
// production caller passes zero and therefore accepts only root-owned paths.
func loadApproverPublicKeysOwnedBy(path string, allowedOwnerUID uint32) (map[string]ed25519.PublicKey, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("operator approver key directory must be a clean absolute path")
	}
	if err := safefile.ValidateNoSymlinkPath(path); err != nil {
		return nil, errors.New("operator approver key directory must not traverse a symlink")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, errors.New("operator approver key directory is unavailable")
		}
		if err := requireApprovalPathOwner(current, info, allowedOwnerUID); err != nil {
			return nil, err
		}
		if info.Mode().Perm()&0o022 != 0 {
			return nil, errors.New("operator approver key directory path is group or world writable")
		}
		if current == path && info.Mode().Perm() != 0o750 {
			return nil, errors.New("operator approver key directory permissions must be 0750")
		}
		if current == "/" {
			break
		}
	}

	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, errors.New("operator approver key directory is unavailable")
	}
	defer root.Close()
	directory, err := root.Open(".")
	if err != nil {
		return nil, errors.New("operator approver key directory is unreadable")
	}
	entries, readErr := directory.ReadDir(maxApproverPublicKeyFiles + 1)
	_ = directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, errors.New("operator approver key directory is unreadable")
	}
	if len(entries) > maxApproverPublicKeyFiles {
		return nil, fmt.Errorf("operator approver key directory contains more than %d files", maxApproverPublicKeyFiles)
	}

	keys := make(map[string]ed25519.PublicKey, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, approverPublicKeyFileSuffix) {
			return nil, fmt.Errorf("operator approver key directory contains unexpected entry %q", name)
		}
		pathInfo, err := root.Lstat(name)
		if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
			return nil, fmt.Errorf("operator approver public key %q is not a regular non-symlink file", name)
		}
		if err := requireApprovalPathOwner(name, pathInfo, allowedOwnerUID); err != nil {
			return nil, err
		}
		if pathInfo.Mode().Perm() != 0o440 {
			return nil, fmt.Errorf("operator approver public key %q permissions must be 0440", name)
		}
		file, err := root.Open(name)
		if err != nil {
			return nil, fmt.Errorf("operator approver public key %q is unavailable", name)
		}
		info, statErr := file.Stat()
		data, readErr := io.ReadAll(io.LimitReader(file, ed25519.PublicKeySize+1))
		closeErr := file.Close()
		if statErr != nil || !os.SameFile(pathInfo, info) || readErr != nil || closeErr != nil {
			return nil, fmt.Errorf("operator approver public key %q changed while opening", name)
		}
		if len(data) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("operator approver public key %q must contain exactly %d bytes", name, ed25519.PublicKeySize)
		}
		publicKey := ed25519.PublicKey(bytes.Clone(data))
		clear(data)
		id := approverKeyID(publicKey)
		if _, duplicate := keys[id]; duplicate {
			return nil, errors.New("operator approver key directory contains duplicate public keys")
		}
		keys[id] = publicKey
	}
	if len(keys) == 0 {
		return nil, errors.New("operator mode requires at least one approver public key")
	}
	return keys, nil
}

func mergeApproverPublicKeys(
	active map[string]ed25519.PublicKey,
	history map[string]ed25519.PublicKey,
) (map[string]ed25519.PublicKey, error) {
	merged := make(map[string]ed25519.PublicKey, len(active)+len(history))
	for id, publicKey := range history {
		merged[id] = ed25519.PublicKey(bytes.Clone(publicKey))
	}
	for id, publicKey := range active {
		if historical, ok := merged[id]; ok && !bytes.Equal(historical, publicKey) {
			return nil, errors.New("approver key ID identifies different public keys")
		}
		merged[id] = ed25519.PublicKey(bytes.Clone(publicKey))
	}
	return merged, nil
}

func requireApprovalPathOwner(path string, info os.FileInfo, allowedOwnerUID uint32) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != 0 && stat.Uid != allowedOwnerUID) {
		return fmt.Errorf("operator approval path %s must be root-owned", path)
	}
	return nil
}

func encodeCanonical(value any) ([]byte, error) {
	return approvalEncMode.Marshal(value)
}

func decodeCanonical(data []byte, out any) error {
	if err := approvalDecMode.Unmarshal(data, out); err != nil {
		return err
	}
	canonical, err := approvalEncMode.Marshal(out)
	if err != nil || !bytes.Equal(data, canonical) {
		return errors.New("CBOR is not in canonical deterministic form")
	}
	return nil
}

func encodeApprovalChallenge(claims approvalClaims, status serviceStatus) (string, error) {
	envelope := approvalChallenge{
		Version: approvalVersion,
		Claims:  claims,
		Status:  approvalStatusForDisplay(status),
	}
	data, err := encodeCanonical(envelope)
	if err != nil {
		return "", err
	}
	return approvalTokenPrefix + "." + base64.RawURLEncoding.EncodeToString(data), nil
}

func approvalStatusForDisplay(status serviceStatus) approvalDisplayStatus {
	return approvalDisplayStatus(status)
}

func (s approvalDisplayStatus) serviceStatus() serviceStatus {
	return serviceStatus(s)
}

func decodeApprovalChallenge(challenge string, now time.Time) (approvalChallenge, error) {
	if len(challenge) == 0 || len(challenge) > maxApprovalChallengeBytes {
		return approvalChallenge{}, errors.New("approval challenge is missing or too large")
	}
	prefix, payload, ok := strings.Cut(challenge, ".")
	if !ok || prefix != approvalTokenPrefix || strings.Contains(payload, ".") {
		return approvalChallenge{}, errors.New("approval challenge is malformed")
	}
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || len(data) > maxApprovalChallengeBytes {
		return approvalChallenge{}, errors.New("approval challenge is malformed")
	}
	var envelope approvalChallenge
	if err := decodeCanonical(data, &envelope); err != nil {
		return approvalChallenge{}, errors.New("approval challenge payload is invalid")
	}
	if envelope.Version != approvalVersion {
		return approvalChallenge{}, errors.New("approval challenge version is unsupported")
	}
	status := envelope.Status.serviceStatus()
	if err := validateAuthorizationClaims(envelope.Claims, now); err != nil {
		return approvalChallenge{}, err
	}
	if status.Unit != envelope.Claims.Unit || status.Scope != envelope.Claims.Scope ||
		serviceStateHash(status) != envelope.Claims.BeforeHash {
		return approvalChallenge{}, errors.New("approval challenge status does not match its claims")
	}
	if err := sanitizeApprovalDisplayStatus(&envelope.Status); err != nil {
		return approvalChallenge{}, err
	}
	return envelope, nil
}

func sanitizeApprovalDisplayStatus(status *approvalDisplayStatus) error {
	values := []*string{
		&status.LoadState,
		&status.ActiveState,
		&status.SubState,
		&status.Result,
		&status.InvocationID,
		&status.Job,
	}
	for _, value := range values {
		if strings.IndexFunc(*value, unicode.IsControl) >= 0 {
			return errors.New("approval challenge status contains terminal control characters")
		}
		*value = boundedSystemdValue(*value)
	}
	return nil
}

func validateAuthorizationClaims(claims approvalClaims, now time.Time) error {
	if claims.Version != approvalVersion || claims.Domain != serviceApprovalDomain {
		return errors.New("approval authorization domain or version is invalid")
	}
	if !approvalIDPattern.MatchString(claims.ServerSession) ||
		!approvalIDPattern.MatchString(claims.TargetID) ||
		!approvalIDPattern.MatchString(claims.ActionID) ||
		!approvalIDPattern.MatchString(claims.ApproverKeyID) {
		return errors.New("approval authorization identity is invalid")
	}
	if _, err := parseServiceAction(string(claims.Action)); err != nil {
		return err
	}
	if ValidateSystemdServiceUnit(claims.Unit) != nil ||
		(claims.Scope != "system" && claims.Scope != "user") ||
		claims.BeforeHash == "" ||
		claims.Nonce == ([approvalNonceBytes]byte{}) {
		return errors.New("approval authorization payload is invalid")
	}
	return validateApprovalTime(claims.IssuedAtUnix, claims.ExpiresAtUnix, now)
}

func validateApprovalTime(issuedAt, expiresAt int64, now time.Time) error {
	nowUnix := now.Unix()
	latestIssuedAt := nowUnix
	if latestIssuedAt <= math.MaxInt64-5 {
		latestIssuedAt += 5
	} else {
		latestIssuedAt = math.MaxInt64
	}
	maxTTL := int64(MaxApprovalTTLSeconds)
	minTTL := int64(MinApprovalTTLSeconds)
	if issuedAt <= 0 || issuedAt > latestIssuedAt || expiresAt <= nowUnix ||
		expiresAt <= issuedAt || issuedAt > math.MaxInt64-maxTTL ||
		expiresAt > issuedAt+maxTTL || expiresAt < issuedAt+minTTL {
		return errors.New("approval token is expired or has an invalid lifetime")
	}
	return nil
}

// InspectServiceChallenge verifies and returns only display fields. It never
// receives a private key and cannot sign an approval.
func InspectServiceChallenge(challenge string, now time.Time) (ServiceApprovalSummary, error) {
	envelope, err := decodeApprovalChallenge(challenge, now)
	if err != nil {
		return ServiceApprovalSummary{}, err
	}
	claims := envelope.Claims
	return ServiceApprovalSummary{
		Action:        string(claims.Action),
		Unit:          claims.Unit,
		Scope:         claims.Scope,
		TargetID:      claims.TargetID,
		ActionID:      claims.ActionID,
		ApproverKeyID: claims.ApproverKeyID,
		ExpiresAt:     claims.ExpiresAtUnix,
		Status: ServiceApprovalStatus{
			LoadState:   envelope.Status.LoadState,
			ActiveState: envelope.Status.ActiveState,
			SubState:    envelope.Status.SubState,
			NRestarts:   envelope.Status.NRestarts,
			MainPID:     envelope.Status.MainPID,
		},
		Consequence: serviceActionConsequence(claims.Action),
	}, nil
}

func serviceActionConsequence(action serviceAction) string {
	switch action {
	case actionStart:
		return "Starts the fixed Mithril service."
	case actionStop:
		return "Stops the fixed Mithril service and interrupts node availability."
	case actionRestart:
		return "Restarts the fixed Mithril service and briefly interrupts node availability."
	default:
		return "Changes the fixed Mithril service state."
	}
}

func approvalSigningMessage(domain string, claimsCBOR []byte) []byte {
	message := make([]byte, 0, len(domain)+1+len(claimsCBOR))
	message = append(message, domain...)
	message = append(message, 0)
	message = append(message, claimsCBOR...)
	return message
}

func encodeSignedApproval(domain, keyID string, claims any, privateKey ed25519.PrivateKey) (string, []byte, error) {
	claimsCBOR, err := encodeCanonical(claims)
	if err != nil {
		return "", nil, err
	}
	signature := ed25519.Sign(privateKey, approvalSigningMessage(domain, claimsCBOR))
	token := strings.Join([]string{
		approvalTokenPrefix,
		keyID,
		base64.RawURLEncoding.EncodeToString(claimsCBOR),
		base64.RawURLEncoding.EncodeToString(signature),
	}, ".")
	return token, claimsCBOR, nil
}

// ApproveServiceChallenge signs both the bearer authorization and its
// non-authorizing audit attestation. Callers must invoke it only after exact
// interactive confirmation.
func ApproveServiceChallenge(challenge string, privateKey ed25519.PrivateKey, now time.Time) (ServiceApprovalBundle, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return ServiceApprovalBundle{}, errors.New("approver private key is invalid")
	}
	envelope, err := decodeApprovalChallenge(challenge, now)
	if err != nil {
		return ServiceApprovalBundle{}, err
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	keyID := approverKeyID(publicKey)
	if keyID != envelope.Claims.ApproverKeyID {
		return ServiceApprovalBundle{}, errors.New("challenge names a different approver key")
	}
	authorization, authorizationCBOR, err := encodeSignedApproval(
		serviceApprovalDomain, keyID, envelope.Claims, privateKey,
	)
	if err != nil {
		return ServiceApprovalBundle{}, errors.New("failed to encode approval authorization")
	}
	authorizationHash := sha256.Sum256(authorizationCBOR)
	nonceHash := sha256.Sum256(envelope.Claims.Nonce[:])
	auditClaims := approvalAuditClaims{
		Version:                   approvalVersion,
		Domain:                    serviceApprovalAuditDomain,
		AuthorizationClaimsSHA256: authorizationHash,
		ServerSession:             envelope.Claims.ServerSession,
		TargetID:                  envelope.Claims.TargetID,
		ActionID:                  envelope.Claims.ActionID,
		Action:                    envelope.Claims.Action,
		Unit:                      envelope.Claims.Unit,
		Scope:                     envelope.Claims.Scope,
		BeforeHash:                envelope.Claims.BeforeHash,
		NonceSHA256:               nonceHash,
		IssuedAtUnix:              envelope.Claims.IssuedAtUnix,
		ExpiresAtUnix:             envelope.Claims.ExpiresAtUnix,
		ApproverKeyID:             keyID,
	}
	attestation, _, err := encodeSignedApproval(
		serviceApprovalAuditDomain, keyID, auditClaims, privateKey,
	)
	if err != nil {
		return ServiceApprovalBundle{}, errors.New("failed to encode approval audit attestation")
	}
	return ServiceApprovalBundle{
		AuthorizationToken: authorization,
		AuditAttestation:   attestation,
	}, nil
}

type signedApprovalParts struct {
	KeyID      string
	ClaimsCBOR []byte
	Proof      []byte
}

func decodeSignedApproval[T any](
	token, expectedDomain string,
	keys map[string]ed25519.PublicKey,
) (T, signedApprovalParts, error) {
	var zero T
	if len(token) == 0 || len(token) > maxApprovalTokenBytes {
		return zero, signedApprovalParts{}, errors.New("approval token is missing or too large")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != approvalTokenPrefix || !approvalIDPattern.MatchString(parts[1]) {
		return zero, signedApprovalParts{}, errors.New("approval token is malformed")
	}
	keyID := parts[1]
	publicKey, ok := keys[keyID]
	if !ok || len(publicKey) != ed25519.PublicKeySize || approverKeyID(publicKey) != keyID {
		return zero, signedApprovalParts{}, errors.New("approval token signer is not configured")
	}
	claimsCBOR, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(claimsCBOR) > maxApprovalTokenBytes {
		return zero, signedApprovalParts{}, errors.New("approval token is malformed")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(signature) != ed25519.SignatureSize {
		return zero, signedApprovalParts{}, errors.New("approval token is malformed")
	}
	var claims T
	if err := decodeCanonical(claimsCBOR, &claims); err != nil {
		return zero, signedApprovalParts{}, errors.New("approval token payload is invalid")
	}
	if !ed25519.Verify(publicKey, approvalSigningMessage(expectedDomain, claimsCBOR), signature) {
		return zero, signedApprovalParts{}, errors.New("approval token signature is invalid")
	}
	return claims, signedApprovalParts{
		KeyID:      keyID,
		ClaimsCBOR: claimsCBOR,
		Proof:      signature,
	}, nil
}

func verifyServiceApprovalBundle(
	bundle ServiceApprovalBundle,
	authority approvalAuthority,
	now time.Time,
) (approvalClaims, ControlApprovalEvidence, error) {
	if !authority.configured() {
		return approvalClaims{}, ControlApprovalEvidence{}, errors.New("operator approval authority is unavailable")
	}
	authorization, authorizationParts, err := decodeSignedApproval[approvalClaims](
		bundle.AuthorizationToken, serviceApprovalDomain, authority.publicKeys,
	)
	if err != nil {
		return approvalClaims{}, ControlApprovalEvidence{}, err
	}
	if err := validateAuthorizationClaims(authorization, now); err != nil {
		return approvalClaims{}, ControlApprovalEvidence{}, err
	}
	if authorization.ApproverKeyID != authorizationParts.KeyID {
		return approvalClaims{}, ControlApprovalEvidence{}, errors.New("approval authorization signer and key ID do not match")
	}
	if authorization.ServerSession != authority.serverSession ||
		authorization.TargetID != authority.targetID {
		return approvalClaims{}, ControlApprovalEvidence{}, errors.New("approval token does not match this server session and target")
	}
	audit, auditParts, err := decodeSignedApproval[approvalAuditClaims](
		bundle.AuditAttestation, serviceApprovalAuditDomain, authority.publicKeys,
	)
	if err != nil {
		return approvalClaims{}, ControlApprovalEvidence{}, err
	}
	if err := validateAuditClaims(audit, now); err != nil {
		return approvalClaims{}, ControlApprovalEvidence{}, err
	}
	if audit.ApproverKeyID != auditParts.KeyID {
		return approvalClaims{}, ControlApprovalEvidence{}, errors.New("approval audit signer and key ID do not match")
	}
	authorizationHash := sha256.Sum256(authorizationParts.ClaimsCBOR)
	nonceHash := sha256.Sum256(authorization.Nonce[:])
	if audit.AuthorizationClaimsSHA256 != authorizationHash ||
		audit.ServerSession != authorization.ServerSession ||
		audit.TargetID != authorization.TargetID ||
		audit.ActionID != authorization.ActionID ||
		audit.Action != authorization.Action ||
		audit.Unit != authorization.Unit ||
		audit.Scope != authorization.Scope ||
		audit.BeforeHash != authorization.BeforeHash ||
		audit.NonceSHA256 != nonceHash ||
		audit.IssuedAtUnix != authorization.IssuedAtUnix ||
		audit.ExpiresAtUnix != authorization.ExpiresAtUnix ||
		audit.ApproverKeyID != authorization.ApproverKeyID {
		return approvalClaims{}, ControlApprovalEvidence{}, errors.New("approval authorization and audit attestation do not match")
	}
	evidence := ControlApprovalEvidence{
		Version:                   audit.Version,
		Domain:                    audit.Domain,
		AuthorizationClaimsSHA256: audit.AuthorizationClaimsSHA256,
		ActionID:                  audit.ActionID,
		ApproverKeyID:             audit.ApproverKeyID,
		NonceSHA256:               audit.NonceSHA256,
		IssuedAtUnix:              audit.IssuedAtUnix,
		ExpiresAtUnix:             audit.ExpiresAtUnix,
		ClaimsCBOR:                bytes.Clone(auditParts.ClaimsCBOR),
		Proof:                     bytes.Clone(auditParts.Proof),
	}
	evidence.EvidenceSHA256 = approvalEvidenceHash(evidence.Domain, evidence.ClaimsCBOR, evidence.Proof)
	return authorization, evidence, nil
}

func approvalEvidenceHash(domain string, claimsCBOR, proof []byte) [32]byte {
	digest := sha256.New()
	_, _ = digest.Write([]byte(domain))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(claimsCBOR)
	_, _ = digest.Write(proof)
	var sum [32]byte
	copy(sum[:], digest.Sum(nil))
	return sum
}

// VerifyControlApprovalEvidence verifies the signature, canonical claims and
// immutable binding of historical non-authorizing evidence. It intentionally
// does not compare the signed lifetime with the wall clock or an event time.
func VerifyControlApprovalEvidence(
	evidence ControlApprovalEvidence,
	publicKeys map[string]ed25519.PublicKey,
) (ControlApprovalBinding, error) {
	if evidence.Version != approvalVersion ||
		evidence.Domain != serviceApprovalAuditDomain ||
		len(evidence.ClaimsCBOR) == 0 ||
		len(evidence.ClaimsCBOR) > maxApprovalTokenBytes ||
		len(evidence.Proof) != ed25519.SignatureSize ||
		evidence.EvidenceSHA256 != approvalEvidenceHash(evidence.Domain, evidence.ClaimsCBOR, evidence.Proof) {
		return ControlApprovalBinding{}, errors.New("approval audit evidence is invalid")
	}
	var claims approvalAuditClaims
	if err := decodeCanonical(evidence.ClaimsCBOR, &claims); err != nil {
		return ControlApprovalBinding{}, errors.New("approval audit evidence claims are invalid")
	}
	if err := validateAuditClaimFields(claims); err != nil {
		return ControlApprovalBinding{}, err
	}
	if evidence.Version != claims.Version ||
		evidence.Domain != claims.Domain ||
		evidence.AuthorizationClaimsSHA256 != claims.AuthorizationClaimsSHA256 ||
		evidence.ActionID != claims.ActionID ||
		evidence.ApproverKeyID != claims.ApproverKeyID ||
		evidence.NonceSHA256 != claims.NonceSHA256 ||
		evidence.IssuedAtUnix != claims.IssuedAtUnix ||
		evidence.ExpiresAtUnix != claims.ExpiresAtUnix {
		return ControlApprovalBinding{}, errors.New("approval audit evidence does not match its canonical claims")
	}
	publicKey, ok := publicKeys[claims.ApproverKeyID]
	if !ok || len(publicKey) != ed25519.PublicKeySize ||
		approverKeyID(publicKey) != claims.ApproverKeyID {
		return ControlApprovalBinding{}, errors.New("approval audit signer is not configured")
	}
	if !ed25519.Verify(
		publicKey,
		approvalSigningMessage(serviceApprovalAuditDomain, evidence.ClaimsCBOR),
		evidence.Proof,
	) {
		return ControlApprovalBinding{}, errors.New("approval audit proof is invalid")
	}
	return ControlApprovalBinding{
		ServerSession:             claims.ServerSession,
		TargetID:                  claims.TargetID,
		ActionID:                  claims.ActionID,
		Action:                    string(claims.Action),
		Unit:                      claims.Unit,
		Scope:                     claims.Scope,
		BeforeHash:                claims.BeforeHash,
		ApproverKeyID:             claims.ApproverKeyID,
		AuthorizationClaimsSHA256: claims.AuthorizationClaimsSHA256,
		NonceSHA256:               claims.NonceSHA256,
		IssuedAtUnix:              claims.IssuedAtUnix,
		ExpiresAtUnix:             claims.ExpiresAtUnix,
	}, nil
}

// ValidateControlApprovalFirstEventTime checks that an action's first durable
// event was created while its verified approval was valid. Later events for
// the same action may occur after expiry and must not call this function.
//
// Nothing in this repository calls it, and that is deliberate rather than an
// oversight — it has been mistaken for an unwired security check twice, so the
// reason is recorded here. The in-tree audit path enforces the same rule at the
// point it matters: controlaudit's validateActionTransition applies the
// identical window comparison to the binding this package hands it, at
// chain-verification time, on every append and every replay. Wiring this in
// beside it would be a second copy of one rule, and copies drift.
//
// It stays exported because VerifyControlApprovalEvidence deliberately does not
// compare a signed lifetime against any clock, so an EXTERNAL verifier — one
// reading an exported chain without controlaudit — has no other way to make
// that comparison correctly. Deleting it would remove the only supported means
// of doing so. Before removing it, confirm no out-of-tree auditor depends on it.
func ValidateControlApprovalFirstEventTime(
	binding ControlApprovalBinding,
	eventTime time.Time,
) error {
	if err := validateApprovalAuditLifetime(
		binding.IssuedAtUnix,
		binding.ExpiresAtUnix,
	); err != nil {
		return err
	}
	eventUnix := eventTime.UTC().Unix()
	if eventUnix < binding.IssuedAtUnix || eventUnix >= binding.ExpiresAtUnix {
		return errors.New("approval audit first event is outside its signed validity window")
	}
	return nil
}

func validateAuditClaims(claims approvalAuditClaims, now time.Time) error {
	if err := validateAuditClaimFields(claims); err != nil {
		return err
	}
	return validateApprovalTime(claims.IssuedAtUnix, claims.ExpiresAtUnix, now)
}

func validateAuditClaimFields(claims approvalAuditClaims) error {
	if claims.Version != approvalVersion || claims.Domain != serviceApprovalAuditDomain {
		return errors.New("approval audit domain or version is invalid")
	}
	if !approvalIDPattern.MatchString(claims.ServerSession) ||
		!approvalIDPattern.MatchString(claims.TargetID) ||
		!approvalIDPattern.MatchString(claims.ActionID) ||
		!approvalIDPattern.MatchString(claims.ApproverKeyID) {
		return errors.New("approval audit identity is invalid")
	}
	if _, err := parseServiceAction(string(claims.Action)); err != nil {
		return err
	}
	if ValidateSystemdServiceUnit(claims.Unit) != nil ||
		(claims.Scope != "system" && claims.Scope != "user") ||
		claims.BeforeHash == "" ||
		claims.AuthorizationClaimsSHA256 == ([32]byte{}) ||
		claims.NonceSHA256 == ([32]byte{}) {
		return errors.New("approval audit payload is invalid")
	}
	return validateApprovalAuditLifetime(claims.IssuedAtUnix, claims.ExpiresAtUnix)
}

func validateApprovalAuditLifetime(issuedAtUnix, expiresAtUnix int64) error {
	maxTTL := int64(MaxApprovalTTLSeconds)
	minTTL := int64(MinApprovalTTLSeconds)
	if issuedAtUnix <= 0 ||
		expiresAtUnix <= issuedAtUnix ||
		issuedAtUnix > math.MaxInt64-maxTTL ||
		expiresAtUnix > issuedAtUnix+maxTTL ||
		expiresAtUnix < issuedAtUnix+minTTL {
		return errors.New("approval audit lifetime is invalid")
	}
	return nil
}
