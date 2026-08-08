package mcp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/internal/controlaudit"
)

func requireControlAuditSummary(t testing.TB, store *controlaudit.Store) controlaudit.Summary {
	t.Helper()
	summary, err := store.Summary()
	if err != nil {
		t.Fatalf("control audit summary: %v", err)
	}
	return summary
}

func controlAuditTempDir(t *testing.T) string {
	t.Helper()
	return secureTempDir(t)
}

func testControlAuditEvidence(
	t *testing.T,
	now time.Time,
) (approvalClaims, serviceStatus, approvalAuthority, ControlApprovalEvidence) {
	t.Helper()
	claims, status, authority, _, bundle := approvalFixture(t, now)
	_, evidence, err := verifyServiceApprovalBundle(bundle, authority, now)
	if err != nil {
		t.Fatal(err)
	}
	return claims, status, authority, evidence
}

func testControlAuditOperation(
	t *testing.T,
	now time.Time,
) (serviceOperation, approvalAuthority) {
	t.Helper()
	claims, status, authority, evidence := testControlAuditEvidence(t, now)
	operation, err := newServiceOperation(
		claims.ActionID,
		claims.ServerSession,
		claims.TargetID,
		claims.Action,
		status,
		evidence,
		now,
		now.Add(2*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	return operation, authority
}

func setTestControlAuditOperationPhase(
	t *testing.T,
	operation *serviceOperation,
	phase operationPhase,
	at time.Time,
) {
	t.Helper()
	previousPhase := operation.Phase
	operation.Phase = phase
	operation.UpdatedAtUnix = at.UTC().Unix()
	if previousPhase == phaseDispatchStarted &&
		(phase == phaseDispatched || phase == phaseOutcomeUnknown) {
		operation.DeadlineUnix = at.Add(postconditionTimeout).UTC().Unix()
	}
	operation.StatusAfter = nil
	operation.AfterHash = ""
	operation.Outcome = ""
	operation.ReasonCode = ""
	operation.DispatchMayHaveOccurred = false
	operation.DispatchAccepted = false
	switch phase {
	case phasePrepared, phaseDispatchStarted:
	case phaseDispatched:
		operation.DispatchMayHaveOccurred = true
		operation.DispatchAccepted = true
	case phaseVerifying:
		operation.DispatchMayHaveOccurred = true
		operation.DispatchAccepted = true
	case phaseSucceeded, phaseFailed:
		status := operation.StatusBefore
		operation.StatusAfter = &status
		operation.AfterHash = serviceStateHash(status)
		operation.Outcome = string(phase)
		operation.ReasonCode = "postcondition_observed"
		operation.DispatchMayHaveOccurred = phase == phaseSucceeded
		operation.DispatchAccepted = phase == phaseSucceeded
	case phaseOutcomeUnknown:
		operation.Outcome = string(phase)
		operation.ReasonCode = "postcondition_deadline"
		operation.DispatchMayHaveOccurred = true
	default:
		t.Fatalf("unsupported test phase %q", phase)
	}
}

func testControlAuditFields(operation serviceOperation, at time.Time) controlAuditEventFields {
	return controlAuditEventFields{
		Timestamp:  at,
		AfterHash:  operation.AfterHash,
		Outcome:    operation.Outcome,
		ReasonCode: operation.ReasonCode,
	}
}

func writeControlAuditTLSFixture(t *testing.T, dir string) (certPath, keyPath, caPath string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "control-audit-test"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	certPath = filepath.Join(dir, "client-cert.pem")
	keyPath = filepath.Join(dir, "client-key.pem")
	caPath = filepath.Join(dir, "server-ca.pem")
	if err := os.WriteFile(certPath, certificatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, privatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, certificatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath, caPath
}

func testControlAuditClientConfig(
	t *testing.T,
	dir string,
) (string, controlAuditClientFile) {
	t.Helper()
	certPath, keyPath, caPath := writeControlAuditTLSFixture(t, dir)
	pin := sha256.Sum256([]byte("fixed-test-server-key"))
	config := controlAuditClientFile{
		Version:               controlAuditClientConfigVersion,
		Endpoint:              "https://127.0.0.1:14443",
		ServerName:            "audit.test",
		ServerSPKIPin:         hex.EncodeToString(pin[:]),
		ClientCertificatePath: certPath,
		ClientPrivateKeyPath:  keyPath,
		ServerCAPath:          caPath,
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit-client.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, config
}

func TestControlAuditClientConfigUsesTrustedBoundedFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("trusted file reads intentionally fail closed on Windows")
	}
	dir := controlAuditTempDir(t)
	path, config := testControlAuditClientConfig(t, dir)
	client, err := loadControlAuditClient(path)
	if err != nil {
		t.Fatal(err)
	}
	client.Close()

	validRaw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		raw  []byte
	}{
		{"unknown field", append(bytes.TrimSuffix(validRaw, []byte("}")), []byte(`,"extra":true}`)...)},
		{"duplicate field", bytes.Replace(validRaw, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)},
		{"trailing value", append(bytes.Clone(validRaw), []byte(`{}`)...)},
		{"wrong version", bytes.Replace(validRaw, []byte(`"version":1`), []byte(`"version":2`), 1)},
		{"relative private key", bytes.Replace(validRaw, []byte(config.ClientPrivateKeyPath), []byte("relative-key.pem"), 1)},
		{"invalid server name", bytes.Replace(validRaw, []byte(config.ServerName), []byte("https://audit.test"), 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(path, test.raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadControlAuditClient(path); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}

	if err := os.WriteFile(path, validRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o622); err != nil {
		t.Fatal(err)
	}
	if _, err := loadControlAuditClient(path); err == nil {
		t.Fatal("group-writable configuration was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(config.ClientPrivateKeyPath, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := loadControlAuditClient(path); err == nil {
		t.Fatal("group-readable private key was accepted")
	}
	if err := os.Chmod(config.ClientPrivateKeyPath, 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "audit-client-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadControlAuditClient(link); err == nil {
		t.Fatal("symlinked configuration was accepted")
	}

	ancestorLink := filepath.Join(dir, "ancestor-link")
	if err := os.Symlink(dir, ancestorLink); err != nil {
		t.Fatal(err)
	}
	if _, err := loadControlAuditClient(filepath.Join(ancestorLink, filepath.Base(path))); err == nil {
		t.Fatal("configuration beneath a symlinked ancestor was accepted")
	}

	config.ClientPrivateKeyPath = filepath.Join(
		ancestorLink,
		filepath.Base(config.ClientPrivateKeyPath),
	)
	ancestorTLSRaw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, ancestorTLSRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadControlAuditClient(path); err == nil {
		t.Fatal("TLS material beneath a symlinked ancestor was accepted")
	}
}

func TestControlAuditEventSealsExactServiceOperationBinding(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	operation, authority := testControlAuditOperation(t, now)
	event, err := controlAuditEvent(
		operation,
		testControlAuditFields(operation, now),
		controlaudit.Summary{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	if event.SessionID != operation.ServerSession ||
		event.TargetID != operation.TargetID ||
		event.ActionID != operation.ID ||
		event.Action != controlaudit.Action(operation.Action) ||
		event.Unit != operation.Unit ||
		event.Scope != operation.Scope ||
		event.BeforeHash != operation.BeforeHash ||
		event.ApproverKeyID != operation.Approval.ApproverKeyID {
		t.Fatalf("event does not match operation: %+v", event)
	}
	if len(event.BeforeHash) != sha256.Size*2 ||
		event.BeforeHash != strings.ToLower(event.BeforeHash) {
		t.Fatalf("before hash is not lowercase SHA-256 hex: %q", event.BeforeHash)
	}
	if _, err := hex.DecodeString(event.BeforeHash); err != nil {
		t.Fatalf("before hash is not hexadecimal: %v", err)
	}
	binding, err := newControlAuditApprovalVerifier(
		authority.publicKeys,
	).VerifyApproval(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if binding.BeforeHash != event.BeforeHash ||
		binding.SessionID != event.SessionID ||
		binding.TargetID != event.TargetID ||
		binding.ActionID != event.ActionID {
		t.Fatalf("approval binding does not match event: %+v", binding)
	}
	if !operationMatchesEvent(operation, event) {
		t.Fatal("sealed event did not match its source operation")
	}
	tampered := event
	tampered.TargetID = "different-target"
	tampered, err = controlaudit.Seal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if operationMatchesEvent(operation, tampered) {
		t.Fatal("event with a changed result matched its source operation")
	}
	if _, err := controlAuditEvent(
		operation,
		testControlAuditFields(operation, now.Add(time.Nanosecond)),
		controlaudit.Summary{},
	); err == nil {
		t.Fatal("timestamp not represented in durable operation state was accepted")
	}
}

func TestControlAuditStoreRejectsCheckpointIdentityAndDeadlineDrift(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*serviceOperation)
	}{
		{
			name: "immutable identity",
			mutate: func(operation *serviceOperation) {
				operation.StartedAtUnix++
			},
		},
		{
			name: "reanchored deadline",
			mutate: func(operation *serviceOperation) {
				operation.DeadlineUnix++
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operation, authority := testControlAuditOperation(t, now)
			dir := controlAuditTempDir(t)
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "audit.jsonl")
			store, err := controlaudit.OpenStore(
				path,
				newControlAuditApprovalVerifier(authority.publicKeys),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			appendOperation := func(operation serviceOperation) controlaudit.Event {
				t.Helper()
				event, err := controlAuditEvent(
					operation,
					testControlAuditFields(
						operation,
						time.Unix(operation.UpdatedAtUnix, 0).UTC(),
					),
					requireControlAuditSummary(t, store),
				)
				if err != nil {
					t.Fatal(err)
				}
				return event
			}

			prepared := appendOperation(operation)
			if _, _, err := store.Append(t.Context(), prepared); err != nil {
				t.Fatal(err)
			}
			setTestControlAuditOperationPhase(
				t,
				&operation,
				phaseDispatchStarted,
				now.Add(time.Second),
			)
			dispatchStarted := appendOperation(operation)
			if _, _, err := store.Append(t.Context(), dispatchStarted); err != nil {
				t.Fatal(err)
			}
			setTestControlAuditOperationPhase(
				t,
				&operation,
				phaseDispatched,
				now.Add(2*time.Second),
			)
			test.mutate(&operation)
			drifted := appendOperation(operation)
			if _, _, err := store.Append(t.Context(), drifted); !errors.Is(
				err,
				controlaudit.ErrInvalidPhase,
			) {
				t.Fatalf("checkpoint drift error = %v", err)
			}
		})
	}
}

func TestControlAuditEvidenceCanonicalBindingAndHistoricalTerminal(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	claims, status, authority, evidence := testControlAuditEvidence(t, now)
	raw, err := marshalControlApprovalEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseControlApprovalEvidence(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(parsed.ClaimsCBOR, evidence.ClaimsCBOR) ||
		!bytes.Equal(parsed.Proof, evidence.Proof) {
		t.Fatal("approval evidence changed during round trip")
	}
	if _, err := parseControlApprovalEvidence(append([]byte(" "), raw...)); err == nil {
		t.Fatal("noncanonical evidence was accepted")
	}

	keys := map[string]ed25519.PublicKey{}
	for id, publicKey := range authority.publicKeys {
		keys[id] = bytes.Clone(publicKey)
	}
	verifier := newControlAuditApprovalVerifier(keys)
	for id := range keys {
		clear(keys[id])
	}
	operation, err := newServiceOperation(
		claims.ActionID,
		claims.ServerSession,
		claims.TargetID,
		claims.Action,
		status,
		evidence,
		now,
		now.Add(2*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	event, err := controlAuditEvent(
		operation,
		testControlAuditFields(operation, now),
		controlaudit.Summary{},
	)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := verifier.VerifyApproval(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	rawHash := sha256.Sum256(raw)
	if binding.SessionID != claims.ServerSession ||
		binding.TargetID != claims.TargetID ||
		binding.ActionID != claims.ActionID ||
		binding.Action != controlaudit.Action(claims.Action) ||
		binding.Unit != claims.Unit ||
		binding.Scope != claims.Scope ||
		binding.BeforeHash != claims.BeforeHash ||
		binding.ApproverKeyID != claims.ApproverKeyID ||
		binding.IssuedAtUnix != claims.IssuedAtUnix ||
		binding.ExpiresAtUnix != claims.ExpiresAtUnix ||
		binding.EvidenceSHA256 != hex.EncodeToString(rawHash[:]) {
		t.Fatalf("approval binding = %+v", binding)
	}

	mismatchedOperation := operation
	mismatchedOperation.UpdatedAtUnix++
	mismatchedCheckpoint, err := marshalControlStateCheckpoint(mismatchedOperation)
	if err != nil {
		t.Fatal(err)
	}
	mismatchedEvent := event
	mismatchedEvent.StateCheckpoint = mismatchedCheckpoint
	mismatchedEvent, err = controlaudit.Seal(mismatchedEvent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.VerifyApproval(context.Background(), mismatchedEvent); err == nil {
		t.Fatal("state checkpoint with a different update time was accepted")
	}

	// Approval proof validation does not compare historical events with the
	// current wall clock. Store applies the signed window only to the initial
	// event, so restoring a later terminal record remains possible.
	setTestControlAuditOperationPhase(
		t,
		&operation,
		phaseFailed,
		now.Add(24*time.Hour),
	)
	terminal, err := controlAuditEvent(
		operation,
		testControlAuditFields(operation, now.Add(24*time.Hour)),
		controlaudit.Summary{
			Records:      1,
			LastSequence: 1,
			TipHash:      event.EventHash,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.VerifyApproval(context.Background(), terminal); err != nil {
		t.Fatalf("historical terminal evidence was rejected: %v", err)
	}

	tampered := evidence
	tampered.Proof = bytes.Clone(tampered.Proof)
	tampered.Proof[0] ^= 0xff
	tampered.EvidenceSHA256 = approvalEvidenceHash(
		tampered.Domain,
		tampered.ClaimsCBOR,
		tampered.Proof,
	)
	tamperedRaw, err := marshalControlApprovalEvidence(tampered)
	if err != nil {
		t.Fatal(err)
	}
	event.ApprovalEvidence = tamperedRaw
	if _, err := verifier.VerifyApproval(context.Background(), event); err == nil {
		t.Fatal("tampered approval evidence was accepted")
	}
}

func TestControlAuditCheckpointContainsNoBearerMaterial(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	claims, status, authority, challenge, bundle := approvalFixture(t, now)
	_, evidence, err := verifyServiceApprovalBundle(bundle, authority, now)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := newServiceOperation(
		claims.ActionID,
		claims.ServerSession,
		claims.TargetID,
		claims.Action,
		status,
		evidence,
		now,
		now.Add(2*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	event, err := controlAuditEvent(
		operation,
		testControlAuditFields(operation, now),
		controlaudit.Summary{},
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, forbidden := range map[string][]byte{
		"challenge":           []byte(challenge),
		"authorization token": []byte(bundle.AuthorizationToken),
		"raw nonce":           claims.Nonce[:],
		"encoded nonce":       []byte(base64.RawURLEncoding.EncodeToString(claims.Nonce[:])),
	} {
		if bytes.Contains(event.StateCheckpoint, forbidden) ||
			bytes.Contains(event.ApprovalEvidence, forbidden) {
			t.Fatalf("control audit persisted %s", name)
		}
	}
}

func TestControlAuditApproverRotationRetainsHistoricalKeys(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("control audit store intentionally fails closed on Windows")
	}
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	claims, status, authority, _, firstBundle := approvalFixture(t, now)
	_, firstEvidence, err := verifyServiceApprovalBundle(firstBundle, authority, now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := newServiceOperation(
		claims.ActionID,
		claims.ServerSession,
		claims.TargetID,
		claims.Action,
		status,
		firstEvidence,
		now,
		now.Add(2*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}

	dir := controlAuditTempDir(t)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, controlAuditStoreName)
	store, err := controlaudit.OpenStore(
		path,
		newControlAuditApprovalVerifier(authority.publicKeys),
	)
	if err != nil {
		t.Fatal(err)
	}
	appendOperation := func(operation serviceOperation) {
		t.Helper()
		event, err := controlAuditEvent(
			operation,
			testControlAuditFields(
				operation,
				time.Unix(operation.UpdatedAtUnix, 0).UTC(),
			),
			requireControlAuditSummary(t, store),
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Append(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	appendOperation(first)
	setTestControlAuditOperationPhase(t, &first, phaseFailed, now.Add(time.Second))
	appendOperation(first)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	secondPublic, secondPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secondClaims := claims
	secondClaims.ActionID = "action-rotated-key"
	secondClaims.ApproverKeyID = approverKeyID(secondPublic)
	secondNow := now.Add(time.Duration(MaxApprovalTTLSeconds)*time.Second + 2*time.Second)
	secondClaims.IssuedAtUnix = secondNow.Unix()
	secondClaims.ExpiresAtUnix = secondNow.Add(time.Minute).Unix()
	secondClaims.Nonce[0] ^= 0xff
	secondBundle := signedBundleForClaims(t, secondClaims, secondPrivate)
	rotatedKeys := make(map[string]ed25519.PublicKey, len(authority.publicKeys)+1)
	for id, publicKey := range authority.publicKeys {
		rotatedKeys[id] = bytes.Clone(publicKey)
	}
	rotatedKeys[secondClaims.ApproverKeyID] = bytes.Clone(secondPublic)
	rotatedAuthority := approvalAuthority{
		publicKeys:    rotatedKeys,
		serverSession: authority.serverSession,
		targetID:      authority.targetID,
	}
	_, secondEvidence, err := verifyServiceApprovalBundle(
		secondBundle,
		rotatedAuthority,
		secondNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newServiceOperation(
		secondClaims.ActionID,
		secondClaims.ServerSession,
		secondClaims.TargetID,
		secondClaims.Action,
		status,
		secondEvidence,
		secondNow,
		secondNow.Add(2*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}

	store, err = controlaudit.OpenStore(
		path,
		newControlAuditApprovalVerifier(rotatedKeys),
	)
	if err != nil {
		t.Fatalf("restore after adding an approver key: %v", err)
	}
	appendOperation(second)
	if summary := requireControlAuditSummary(t, store); summary.Records != 3 {
		t.Fatalf("records after key rotation = %d, want 3", summary.Records)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	newKeyOnly := newControlAuditApprovalVerifier(map[string]ed25519.PublicKey{
		secondClaims.ApproverKeyID: secondPublic,
	})
	_, err = openControlAuditTrailWithRemote(
		context.Background(),
		path,
		newKeyOnly,
		&testControlAuditRemote{},
		nil,
	)
	if err == nil ||
		!strings.Contains(err.Error(), "historical approver public key") ||
		!strings.Contains(err.Error(), "retain every key") {
		t.Fatalf("restore without historical key error = %v", err)
	}
}

type testControlAuditRemote struct {
	mu             sync.Mutex
	calls          int
	summaryCalls   int
	failAfterStore bool
	down           bool
	appendErr      error
	beforeReturn   func(controlaudit.Event)
	events         map[string]controlaudit.Event
	received       []uint64
	downAfter      uint64
	summary        controlaudit.Summary
	summaryResult  *controlaudit.Summary
	summaryErr     error
}

func (remote *testControlAuditRemote) Append(
	_ context.Context,
	event controlaudit.Event,
) (controlaudit.Receipt, error) {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	remote.calls++
	if remote.beforeReturn != nil {
		remote.beforeReturn(event)
	}
	if remote.appendErr != nil {
		return controlaudit.Receipt{}, remote.appendErr
	}
	if remote.down {
		return controlaudit.Receipt{}, errors.New("receiver unavailable")
	}
	if remote.events == nil {
		remote.events = make(map[string]controlaudit.Event)
	}
	if existing, ok := remote.events[event.ID]; ok {
		if existing.EventHash != event.EventHash {
			return controlaudit.Receipt{}, errors.New("conflicting event")
		}
		return controlaudit.Receipt{
			Version:   controlaudit.ProtocolVersion,
			EventID:   event.ID,
			EventHash: event.EventHash,
			Sequence:  event.Sequence,
		}, nil
	}
	remote.events[event.ID] = event
	remote.received = append(remote.received, event.Sequence)
	encoded, err := controlaudit.MarshalEvent(event)
	if err != nil {
		return controlaudit.Receipt{}, err
	}
	remote.summary.Records++
	remote.summary.LastSequence = event.Sequence
	remote.summary.TipHash = event.EventHash
	remote.summary.Bytes += uint64(len(encoded) + 1)
	if remote.downAfter == event.Sequence {
		remote.down = true
	}
	if remote.failAfterStore {
		remote.failAfterStore = false
		return controlaudit.Receipt{}, errors.New("lost acknowledgement")
	}
	return controlaudit.Receipt{
		Version:   controlaudit.ProtocolVersion,
		EventID:   event.ID,
		EventHash: event.EventHash,
		Sequence:  event.Sequence,
	}, nil
}

func (remote *testControlAuditRemote) Summary(
	_ context.Context,
) (controlaudit.Summary, error) {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	remote.summaryCalls++
	if remote.down {
		return controlaudit.Summary{}, errors.New("receiver unavailable")
	}
	if remote.summaryErr != nil {
		return controlaudit.Summary{}, remote.summaryErr
	}
	if remote.summaryResult != nil {
		return *remote.summaryResult, nil
	}
	return remote.summary, nil
}

func TestControlAuditTrailPersistsBeforeRemoteAndRetriesLostAck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("control audit store intentionally fails closed on Windows")
	}
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	operation, authority := testControlAuditOperation(t, now)
	verifier := newControlAuditApprovalVerifier(authority.publicKeys)
	dir := controlAuditTempDir(t)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, controlAuditStoreName)
	remote := &testControlAuditRemote{failAfterStore: true}
	remote.beforeReturn = func(controlaudit.Event) {
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			t.Fatal("remote was called before the local event was durable")
		}
	}
	trail, err := openControlAuditTrailWithRemote(
		context.Background(),
		path,
		verifier,
		remote,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer trail.close()

	event, err := trail.appendAndAcknowledge(
		context.Background(),
		operation,
		testControlAuditFields(operation, now),
	)
	if !errors.Is(err, errControlAuditPending) || event.EventHash == "" {
		t.Fatalf("first append = event %+v, error %v", event, err)
	}
	last, ok := trail.lastEvent()
	if !ok || last.EventHash != event.EventHash {
		t.Fatalf("last event = %+v, %v", last, ok)
	}
	if summary := requireControlAuditSummary(t, trail.store); summary.Records != 1 ||
		summary.TipHash != event.EventHash {
		t.Fatalf("local summary = %+v", summary)
	}
	if err := trail.syncPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(trail.pending) != 0 || remote.calls != 2 {
		t.Fatalf("pending=%d calls=%d", len(trail.pending), remote.calls)
	}

	setTestControlAuditOperationPhase(t, &operation, phaseDispatchStarted, now.Add(time.Second))
	second, err := trail.appendAndAcknowledge(
		context.Background(),
		operation,
		testControlAuditFields(operation, now.Add(time.Second)),
	)
	if err != nil || second.Sequence != 2 {
		t.Fatalf("second append = event %+v, error %v", second, err)
	}
}

func TestControlAuditTrailPersistsPostDispatchDuringRemoteOutage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("control audit store intentionally fails closed on Windows")
	}
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	operation, authority := testControlAuditOperation(t, now)
	verifier := newControlAuditApprovalVerifier(authority.publicKeys)
	dir := controlAuditTempDir(t)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	remote := &testControlAuditRemote{}
	trail, err := openControlAuditTrailWithRemote(
		context.Background(),
		filepath.Join(dir, controlAuditStoreName),
		verifier,
		remote,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer trail.close()

	for _, step := range []struct {
		phase operationPhase
		at    time.Time
	}{
		{phasePrepared, now},
		{phaseDispatchStarted, now.Add(time.Second)},
	} {
		setTestControlAuditOperationPhase(t, &operation, step.phase, step.at)
		if _, err := trail.appendAndAcknowledge(
			context.Background(),
			operation,
			testControlAuditFields(operation, step.at),
		); err != nil {
			t.Fatalf("acknowledge %s: %v", step.phase, err)
		}
	}

	remote.down = true
	for _, step := range []struct {
		phase operationPhase
		at    time.Time
	}{
		{phaseDispatched, now.Add(2 * time.Second)},
		{phaseVerifying, now.Add(70 * time.Second)},
		{phaseOutcomeUnknown, now.Add(71 * time.Second)},
	} {
		setTestControlAuditOperationPhase(t, &operation, step.phase, step.at)
		event, err := trail.appendLocalAndQueue(
			context.Background(),
			operation,
			testControlAuditFields(operation, step.at),
		)
		if !errors.Is(err, errControlAuditPending) || event.EventHash == "" {
			t.Fatalf("queue %s = event %+v, error %v", step.phase, event, err)
		}
	}
	if summary := requireControlAuditSummary(t, trail.store); summary.Records != 5 {
		t.Fatalf("local summary during outage = %+v", summary)
	}
	if len(trail.pending) != 3 {
		t.Fatalf("pending during outage = %d", len(trail.pending))
	}

	remote.down = false
	if err := trail.syncPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(trail.pending) != 0 {
		t.Fatalf("pending after recovery = %d", len(trail.pending))
	}
	want := []uint64{1, 2, 3, 4, 5}
	if !slices.Equal(remote.received, want) {
		t.Fatalf("receiver order = %v, want %v", remote.received, want)
	}
}

func TestControlRuntimeReconstructsStateFromCopiedAudit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("control audit store intentionally fails closed on Windows")
	}
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	controller, remote := testServiceControllerWithRemote(
		t,
		&fakeServiceRunner{status: []byte(activeServiceStatus)},
		now,
	)
	prepared, err := controller.prepare(t.Context(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	executed, err := controller.execute(
		t.Context(),
		approveTestChallenge(t, prepared.Challenge, now),
	)
	if err != nil || executed.Phase != phaseSucceeded {
		t.Fatalf("execute = %+v, %v", executed, err)
	}
	want, err := controller.runtime.state.load()
	if err != nil || want == nil {
		t.Fatalf("source state = %+v, %v", want, err)
	}
	wantRaw, err := marshalControlState(*want)
	if err != nil {
		t.Fatal(err)
	}
	sourceAudit := controller.runtime.audit.path
	if err := controller.runtime.close(); err != nil {
		t.Fatal(err)
	}
	auditBytes, err := os.ReadFile(sourceAudit)
	if err != nil {
		t.Fatal(err)
	}

	restoredDir := controlAuditTempDir(t)
	if err := os.Chmod(restoredDir, 0o700); err != nil {
		t.Fatal(err)
	}
	restoredAudit := filepath.Join(restoredDir, controlAuditStoreName)
	if err := os.WriteFile(restoredAudit, auditBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := newControlStateStore(filepath.Join(restoredDir, "operation.json"))
	if err != nil {
		t.Fatal(err)
	}
	trail, err := openControlAuditTrailWithRemote(
		t.Context(),
		restoredAudit,
		newControlAuditApprovalVerifier(controller.authority.publicKeys),
		remote,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	recovered := &controlRuntime{
		state:        state,
		audit:        trail,
		approvalKeys: controller.authority.publicKeys,
		targetID:     controller.authority.targetID,
		unit:         controller.cfg.SystemdUnit,
		scope:        controller.cfg.SystemdScope,
	}
	t.Cleanup(func() {
		_ = recovered.close()
	})
	if err := recovered.recover(t.Context(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := state.load()
	if err != nil || got == nil {
		t.Fatalf("restored state = %+v, %v", got, err)
	}
	gotRaw, err := marshalControlState(*got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotRaw, wantRaw) {
		t.Fatal("restored state differs from the last verified audit checkpoint")
	}
}

func TestRestoreControlStateSnapshotIsAtomicAndIdempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("control state store intentionally fails closed on Windows")
	}
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	operation, authority := testControlAuditOperation(t, now)
	event, err := controlAuditEvent(
		operation,
		testControlAuditFields(operation, now),
		controlaudit.Summary{},
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := controlaudit.MarshalEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	summary := controlaudit.Summary{
		Records:      1,
		LastSequence: 1,
		TipHash:      event.EventHash,
		Bytes:        uint64(len(encoded) + 1),
	}
	stateDir := controlAuditTempDir(t)
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := ControlRestoreConfig{
		ControlStateDir: stateDir,
		TargetID:        authority.targetID,
		SystemdUnit:     operation.Unit,
		SystemdScope:    operation.Scope,
	}

	results := make(chan ControlRestoreResult, 2)
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := restoreControlStateSnapshot(
				t.Context(),
				config,
				authority.publicKeys,
				[]controlaudit.Event{event},
				summary,
			)
			results <- result
			errs <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errs)
	restored := 0
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for result := range results {
		if result.Records != 1 ||
			result.ActionID != operation.ID ||
			result.Phase != string(phasePrepared) {
			t.Fatalf("restore result = %+v", result)
		}
		if result.StateRestored {
			restored++
		}
	}
	if restored != 1 {
		t.Fatalf("state was created %d times, want once", restored)
	}

	state, err := newControlStateStore(filepath.Join(stateDir, "operation.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := state.load()
	if err != nil || got == nil {
		t.Fatalf("restored state = %+v, %v", got, err)
	}
	gotRaw, err := marshalControlState(*got)
	if err != nil {
		t.Fatal(err)
	}
	wantRaw, err := marshalControlState(operation)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotRaw, wantRaw) {
		t.Fatal("atomic restore wrote different operation state")
	}

	different := operation
	different.DeadlineUnix++
	if err := state.save(different); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreControlStateSnapshot(
		t.Context(),
		config,
		authority.publicKeys,
		[]controlaudit.Event{event},
		summary,
	); err == nil {
		t.Fatal("restore replaced a different existing operation")
	}
}

func TestControlRestoreSummaryRequiresExactReceiverCopy(t *testing.T) {
	local := controlaudit.Summary{
		Records:      3,
		LastSequence: 3,
		TipHash:      strings.Repeat("a", 64),
		Bytes:        1234,
	}
	if err := validateControlRestoreSummary(local, local); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*controlaudit.Summary){
		func(summary *controlaudit.Summary) { summary.Records-- },
		func(summary *controlaudit.Summary) { summary.LastSequence-- },
		func(summary *controlaudit.Summary) { summary.TipHash = strings.Repeat("b", 64) },
		func(summary *controlaudit.Summary) { summary.Bytes-- },
	} {
		remote := local
		mutate(&remote)
		if err := validateControlRestoreSummary(local, remote); err == nil {
			t.Fatalf("mismatched receiver summary was accepted: %+v", remote)
		}
	}
}

func TestControlAuditTrailStartupReplaysAndTracksOnlyLastEvent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("control audit store intentionally fails closed on Windows")
	}
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	operation, authority := testControlAuditOperation(t, now)
	verifier := newControlAuditApprovalVerifier(authority.publicKeys)
	dir := controlAuditTempDir(t)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, controlAuditStoreName)
	firstRemote := &testControlAuditRemote{}
	first, err := openControlAuditTrailWithRemote(
		context.Background(),
		path,
		verifier,
		firstRemote,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	event, err := first.appendAndAcknowledge(
		context.Background(),
		operation,
		testControlAuditFields(operation, now),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}

	secondRemote := &testControlAuditRemote{}
	second, err := openControlAuditTrailWithRemote(
		context.Background(),
		path,
		verifier,
		secondRemote,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	last, ok := second.lastEvent()
	if !ok || last.EventHash != event.EventHash || len(secondRemote.events) != 1 {
		t.Fatalf("replayed last=%+v ok=%v remote=%d", last, ok, len(secondRemote.events))
	}
}

func TestControlAuditTrailStartupUsesRemotePrefix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("control audit store intentionally fails closed on Windows")
	}
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	operation, authority := testControlAuditOperation(t, now)
	verifier := newControlAuditApprovalVerifier(authority.publicKeys)
	dir := controlAuditTempDir(t)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, controlAuditStoreName)
	equalRemote := &testControlAuditRemote{}
	writer, err := openControlAuditTrailWithRemote(
		context.Background(),
		path,
		verifier,
		equalRemote,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		phase operationPhase
		at    time.Time
	}{
		{phasePrepared, now},
		{phaseDispatchStarted, now.Add(time.Second)},
	} {
		setTestControlAuditOperationPhase(t, &operation, step.phase, step.at)
		if _, err := writer.appendAndAcknowledge(
			context.Background(),
			operation,
			testControlAuditFields(operation, step.at),
		); err != nil {
			t.Fatalf("append %s: %v", step.phase, err)
		}
	}
	if err := writer.close(); err != nil {
		t.Fatal(err)
	}
	events, localSummary, err := controlaudit.Restore(context.Background(), path, verifier)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("equal", func(t *testing.T) {
		callsBefore := equalRemote.calls
		trail, err := openControlAuditTrailWithRemote(
			context.Background(),
			path,
			verifier,
			equalRemote,
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer trail.close()
		if equalRemote.calls != callsBefore || equalRemote.summaryCalls == 0 {
			t.Fatalf(
				"equal prefix caused replay: calls=%d before=%d summaries=%d",
				equalRemote.calls,
				callsBefore,
				equalRemote.summaryCalls,
			)
		}
	})

	t.Run("behind", func(t *testing.T) {
		remote := &testControlAuditRemote{}
		if _, err := remote.Append(context.Background(), events[0]); err != nil {
			t.Fatal(err)
		}
		callsBefore := remote.calls
		trail, err := openControlAuditTrailWithRemote(
			context.Background(),
			path,
			verifier,
			remote,
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer trail.close()
		if remote.calls != callsBefore+1 ||
			remote.summary != localSummary ||
			!slices.Equal(remote.received, []uint64{1, 2}) {
			t.Fatalf(
				"behind sync calls=%d summary=%+v received=%v",
				remote.calls,
				remote.summary,
				remote.received,
			)
		}
	})

	t.Run("ahead", func(t *testing.T) {
		ahead := localSummary
		ahead.Records++
		ahead.LastSequence++
		remote := &testControlAuditRemote{summaryResult: &ahead}
		if trail, err := openControlAuditTrailWithRemote(
			context.Background(),
			path,
			verifier,
			remote,
			nil,
		); err == nil {
			_ = trail.close()
			t.Fatal("remote-ahead chain was accepted")
		}
		if remote.calls != 0 {
			t.Fatalf("remote ahead received %d events", remote.calls)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		mismatch := localSummary
		mismatch.TipHash = strings.Repeat("0", sha256.Size*2)
		remote := &testControlAuditRemote{summaryResult: &mismatch}
		if trail, err := openControlAuditTrailWithRemote(
			context.Background(),
			path,
			verifier,
			remote,
			nil,
		); err == nil {
			_ = trail.close()
			t.Fatal("mismatched remote prefix was accepted")
		}
		if remote.calls != 0 {
			t.Fatalf("mismatched remote received %d events", remote.calls)
		}
	})
}

func TestControlAuditTrailStartupHandlesEmptyAndLostAcknowledgement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("control audit store intentionally fails closed on Windows")
	}
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	operation, authority := testControlAuditOperation(t, now)
	verifier := newControlAuditApprovalVerifier(authority.publicKeys)
	dir := controlAuditTempDir(t)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	emptyRemote := &testControlAuditRemote{}
	empty, err := openControlAuditTrailWithRemote(
		context.Background(),
		filepath.Join(dir, "empty.jsonl"),
		verifier,
		emptyRemote,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if emptyRemote.summaryCalls != 1 || emptyRemote.calls != 0 {
		t.Fatalf(
			"empty sync summaries=%d appends=%d",
			emptyRemote.summaryCalls,
			emptyRemote.calls,
		)
	}
	if err := empty.close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "lost-ack.jsonl")
	remote := &testControlAuditRemote{failAfterStore: true}
	first, err := openControlAuditTrailWithRemote(
		context.Background(),
		path,
		verifier,
		remote,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.appendAndAcknowledge(
		context.Background(),
		operation,
		testControlAuditFields(operation, now),
	); !errors.Is(err, errControlAuditPending) {
		t.Fatalf("lost acknowledgement error = %v", err)
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	callsBefore := remote.calls
	second, err := openControlAuditTrailWithRemote(
		context.Background(),
		path,
		verifier,
		remote,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	if remote.calls != callsBefore {
		t.Fatalf(
			"lost acknowledgement replayed an equal prefix: calls=%d before=%d",
			remote.calls,
			callsBefore,
		)
	}
}

func TestControlAuditTrailRestoresTerminalEventsAfterApprovalExpiry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("control audit store intentionally fails closed on Windows")
	}
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	operation, authority := testControlAuditOperation(t, now)
	verifier := newControlAuditApprovalVerifier(authority.publicKeys)
	dir := controlAuditTempDir(t)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, controlAuditStoreName)
	trail, err := openControlAuditTrailWithRemote(
		context.Background(),
		path,
		verifier,
		&testControlAuditRemote{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		phase operationPhase
		at    time.Time
	}{
		{phasePrepared, now},
		{phaseDispatchStarted, now.Add(time.Second)},
		{phaseDispatched, now.Add(2 * time.Second)},
		{phaseVerifying, now.Add(70 * time.Second)},
		{phaseSucceeded, now.Add(71 * time.Second)},
	}
	for _, step := range steps {
		setTestControlAuditOperationPhase(t, &operation, step.phase, step.at)
		if _, err := trail.appendAndAcknowledge(
			context.Background(),
			operation,
			testControlAuditFields(operation, step.at),
		); err != nil {
			t.Fatalf("append %s: %v", step.phase, err)
		}
	}
	if err := trail.close(); err != nil {
		t.Fatal(err)
	}

	restored, summary, err := controlaudit.Restore(context.Background(), path, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != len(steps) ||
		summary.Records != uint64(len(steps)) ||
		restored[len(restored)-1].Phase != controlaudit.PhaseSucceeded {
		t.Fatalf("restored=%d summary=%+v", len(restored), summary)
	}
}
