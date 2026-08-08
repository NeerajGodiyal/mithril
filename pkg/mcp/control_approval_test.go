package mcp

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func approvalFixture(
	t *testing.T,
	now time.Time,
) (approvalClaims, serviceStatus, approvalAuthority, ed25519.PrivateKey, ServiceApprovalBundle) {
	t.Helper()
	privateKey := testApproverPrivateKey()
	authority := testApprovalAuthority()
	status, err := parseServiceStatus("mithril.service", "system", []byte(activeServiceStatus))
	if err != nil {
		t.Fatal(err)
	}
	var nonce [approvalNonceBytes]byte
	copy(nonce[:], bytes.Repeat([]byte{0x33}, approvalNonceBytes))
	claims := approvalClaims{
		Version:       approvalVersion,
		Domain:        serviceApprovalDomain,
		ServerSession: authority.serverSession,
		TargetID:      authority.targetID,
		ActionID:      "action-123",
		Action:        actionRestart,
		Unit:          status.Unit,
		Scope:         status.Scope,
		BeforeHash:    serviceStateHash(status),
		Nonce:         nonce,
		IssuedAtUnix:  now.Unix(),
		ExpiresAtUnix: now.Add(time.Minute).Unix(),
		ApproverKeyID: approverKeyID(privateKey.Public().(ed25519.PublicKey)),
	}
	challenge, err := encodeApprovalChallenge(claims, status)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := ApproveServiceChallenge(challenge, privateKey, now)
	if err != nil {
		t.Fatal(err)
	}
	return claims, status, authority, privateKey, bundle
}

func signedBundleForClaims(
	t *testing.T,
	claims approvalClaims,
	privateKey ed25519.PrivateKey,
) ServiceApprovalBundle {
	t.Helper()
	keyID := approverKeyID(privateKey.Public().(ed25519.PublicKey))
	authorization, authorizationCBOR, err := encodeSignedApproval(
		serviceApprovalDomain,
		keyID,
		claims,
		privateKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	audit := approvalAuditClaims{
		Version:                   approvalVersion,
		Domain:                    serviceApprovalAuditDomain,
		AuthorizationClaimsSHA256: sha256.Sum256(authorizationCBOR),
		ServerSession:             claims.ServerSession,
		TargetID:                  claims.TargetID,
		ActionID:                  claims.ActionID,
		Action:                    claims.Action,
		Unit:                      claims.Unit,
		Scope:                     claims.Scope,
		BeforeHash:                claims.BeforeHash,
		NonceSHA256:               sha256.Sum256(claims.Nonce[:]),
		IssuedAtUnix:              claims.IssuedAtUnix,
		ExpiresAtUnix:             claims.ExpiresAtUnix,
		ApproverKeyID:             claims.ApproverKeyID,
	}
	attestation, _, err := encodeSignedApproval(
		serviceApprovalAuditDomain,
		keyID,
		audit,
		privateKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	return ServiceApprovalBundle{
		AuthorizationToken: authorization,
		AuditAttestation:   attestation,
	}
}

func signedRawApproval(domain, keyID string, claimsCBOR []byte, privateKey ed25519.PrivateKey) string {
	signature := ed25519.Sign(privateKey, approvalSigningMessage(domain, claimsCBOR))
	return strings.Join([]string{
		approvalTokenPrefix,
		keyID,
		base64.RawURLEncoding.EncodeToString(claimsCBOR),
		base64.RawURLEncoding.EncodeToString(signature),
	}, ".")
}

func TestGeneratedApprovalIDsMatchSyntax(t *testing.T) {
	for _, test := range []struct {
		name      string
		firstByte byte
	}{
		{name: "dash", firstByte: 0xf8},
		{name: "underscore", firstByte: 0xfc},
	} {
		t.Run(test.name, func(t *testing.T) {
			unsafeRandom := make([]byte, approvalNonceBytes)
			unsafeRandom[0] = test.firstByte
			randomID, err := randomApprovalID(bytes.NewReader(unsafeRandom))
			if err != nil {
				t.Fatal(err)
			}
			if !approvalIDPattern.MatchString(randomID) || randomID[0] != 'x' {
				t.Fatalf("random approval ID is invalid: %q", randomID)
			}
		})
	}

	publicKey := make(ed25519.PublicKey, ed25519.PublicKeySize)
	publicKey[0] = 71 // Its truncated SHA-256 starts with "_" in base64url.
	keyID := approverKeyID(publicKey)
	if !approvalIDPattern.MatchString(keyID) || keyID[0] != 'x' {
		t.Fatalf("approver key ID is invalid: %q", keyID)
	}

	safeRandom := bytes.Repeat([]byte{0x33}, approvalNonceBytes)
	safeID, err := randomApprovalID(bytes.NewReader(safeRandom))
	if err != nil {
		t.Fatal(err)
	}
	want := base64.RawURLEncoding.EncodeToString(safeRandom)
	if safeID != want {
		t.Fatalf("previously valid approval ID changed: got %q, want %q", safeID, want)
	}
}

func TestApprovalBundleRoundTripAndHistoricalEvidence(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	claims, _, authority, _, bundle := approvalFixture(t, now)

	summary, err := InspectServiceChallenge(mustChallengeForClaims(t, claims), now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.TargetID != claims.TargetID || summary.ActionID != claims.ActionID ||
		summary.Action != string(claims.Action) || summary.Status.ActiveState != "active" ||
		summary.Consequence == "" {
		t.Fatalf("approval summary = %+v", summary)
	}

	approved, evidence, err := verifyServiceApprovalBundle(bundle, authority, now)
	if err != nil {
		t.Fatal(err)
	}
	if approved != claims || evidence.Domain != serviceApprovalAuditDomain ||
		len(evidence.ClaimsCBOR) == 0 || len(evidence.Proof) != ed25519.SignatureSize {
		t.Fatalf("approved=%+v evidence=%+v", approved, evidence)
	}
	encodedEvidence, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encodedEvidence, []byte(bundle.AuthorizationToken)) ||
		bytes.Contains(encodedEvidence, []byte(bundle.AuditAttestation)) {
		t.Fatal("persistable evidence retained a signed token")
	}

	binding, err := VerifyControlApprovalEvidence(
		evidence,
		authority.publicKeys,
	)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ServerSession != claims.ServerSession ||
		binding.TargetID != claims.TargetID ||
		binding.ActionID != claims.ActionID ||
		binding.Action != string(claims.Action) ||
		binding.Unit != claims.Unit ||
		binding.Scope != claims.Scope ||
		binding.BeforeHash != claims.BeforeHash ||
		binding.ApproverKeyID != claims.ApproverKeyID {
		t.Fatalf("evidence binding = %+v", binding)
	}
	if err := ValidateControlApprovalFirstEventTime(binding, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateControlApprovalFirstEventTime(
		binding,
		time.Unix(claims.IssuedAtUnix, 0),
	); err != nil {
		t.Fatalf("event at approval issuance was rejected: %v", err)
	}
	for _, eventTime := range []time.Time{
		now.Add(-time.Second),
		time.Unix(claims.ExpiresAtUnix, 0),
	} {
		if err := ValidateControlApprovalFirstEventTime(binding, eventTime); err == nil {
			t.Errorf("evidence accepted out-of-window event time %s", eventTime)
		}
	}
	invalidLifetime := binding
	invalidLifetime.ExpiresAtUnix = invalidLifetime.IssuedAtUnix
	if err := ValidateControlApprovalFirstEventTime(
		invalidLifetime,
		time.Unix(invalidLifetime.IssuedAtUnix, 0),
	); err == nil {
		t.Fatal("invalid approval lifetime was accepted")
	}
	if _, err := VerifyControlApprovalEvidence(evidence, authority.publicKeys); err != nil {
		t.Fatalf("historical evidence failed after expiry: %v", err)
	}
}

func TestInspectServiceChallengeRejectsControlStatus(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	claims, status, _, _, _ := approvalFixture(t, now)
	status.SubState = "running\x1b[2J\x1b[H\nstart"
	claims.BeforeHash = serviceStateHash(status)
	challenge, err := encodeApprovalChallenge(claims, status)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := InspectServiceChallenge(challenge, now); err == nil {
		t.Fatal("challenge with terminal control bytes was accepted")
	}
}

func mustChallengeForClaims(t *testing.T, claims approvalClaims) string {
	t.Helper()
	status, err := parseServiceStatus(claims.Unit, claims.Scope, []byte(activeServiceStatus))
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := encodeApprovalChallenge(claims, status)
	if err != nil {
		t.Fatal(err)
	}
	return challenge
}

func TestApprovalBundleRejectsWrongAuthorityAndClaims(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	claims, _, authority, privateKey, valid := approvalFixture(t, now)

	otherPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x77}, ed25519.SeedSize))
	otherPublic := otherPrivate.Public().(ed25519.PublicKey)
	otherID := approverKeyID(otherPublic)
	if _, err := ApproveServiceChallenge(mustChallengeForClaims(t, claims), otherPrivate, now); err == nil {
		t.Fatal("challenge was signed by a private key it did not name")
	}
	wrongSignerClaims := claims
	wrongSignerClaims.ApproverKeyID = otherID
	wrongSigner := signedBundleForClaims(t, wrongSignerClaims, otherPrivate)
	claimedOtherKey := signedBundleForClaims(t, wrongSignerClaims, privateKey)
	twoKeyAuthority := authority
	twoKeyAuthority.publicKeys = map[string]ed25519.PublicKey{
		claims.ApproverKeyID: authority.publicKeys[claims.ApproverKeyID],
		otherID:              otherPublic,
	}

	cases := []struct {
		name      string
		bundle    ServiceApprovalBundle
		authority approvalAuthority
		at        time.Time
	}{
		{"wrong signer", wrongSigner, authority, now},
		{"signer differs from claimed key", claimedOtherKey, twoKeyAuthority, now},
		{"wrong session", valid, approvalAuthority{publicKeys: authority.publicKeys, serverSession: "other-session", targetID: authority.targetID}, now},
		{"wrong target", valid, approvalAuthority{publicKeys: authority.publicKeys, serverSession: authority.serverSession, targetID: "other-target"}, now},
		{"expired", valid, authority, now.Add(2 * time.Minute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := verifyServiceApprovalBundle(tc.bundle, tc.authority, tc.at); err == nil {
				t.Fatal("invalid approval bundle was accepted")
			}
		})
	}

	for name, mutate := range map[string]func(*approvalClaims){
		"wrong domain":   func(c *approvalClaims) { c.Domain = serviceApprovalAuditDomain },
		"unknown action": func(c *approvalClaims) { c.Action = serviceAction("reload") },
		"not yet valid": func(c *approvalClaims) {
			c.IssuedAtUnix = now.Add(10 * time.Second).Unix()
			c.ExpiresAtUnix = now.Add(70 * time.Second).Unix()
		},
		"lifetime below minimum": func(c *approvalClaims) {
			c.ExpiresAtUnix = c.IssuedAtUnix + int64(MinApprovalTTLSeconds) - 1
		},
	} {
		t.Run(name, func(t *testing.T) {
			forged := claims
			mutate(&forged)
			bundle := signedBundleForClaims(t, forged, privateKey)
			if _, _, err := verifyServiceApprovalBundle(bundle, authority, now); err == nil {
				t.Fatal("invalid signed claims were accepted")
			}
		})
	}
}

func TestApprovalBundleRejectsTamperingAndCrossUse(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	_, _, authority, _, valid := approvalFixture(t, now)

	altered := valid
	parts := strings.Split(altered.AuthorizationToken, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	parts[2] = base64.RawURLEncoding.EncodeToString(raw)
	altered.AuthorizationToken = strings.Join(parts, ".")

	cases := []ServiceApprovalBundle{
		altered,
		{AuthorizationToken: valid.AuditAttestation, AuditAttestation: valid.AuditAttestation},
		{AuthorizationToken: valid.AuthorizationToken, AuditAttestation: valid.AuthorizationToken},
		{AuthorizationToken: valid.AuthorizationToken + "x", AuditAttestation: valid.AuditAttestation},
	}
	for i, bundle := range cases {
		if _, _, err := verifyServiceApprovalBundle(bundle, authority, now); err == nil {
			t.Errorf("tampered or cross-used bundle %d was accepted", i)
		}
	}
}

func TestApprovalBundleRejectsNoncanonicalDuplicateAndUnknownCBOR(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	claims, _, authority, privateKey, valid := approvalFixture(t, now)
	canonical, err := encodeCanonical(claims)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical) == 0 || canonical[0] != 0xad || canonical[1] != 0x01 {
		t.Fatalf("unexpected canonical claims prefix %x", canonical[:min(len(canonical), 4)])
	}
	keyID := approverKeyID(privateKey.Public().(ed25519.PublicKey))

	noncanonical := append([]byte{canonical[0], 0x18, 0x01}, canonical[2:]...)
	duplicate := append(bytes.Clone(canonical), 0x01, 0x01)
	duplicate[0] = 0xae
	unknown := append(bytes.Clone(canonical), 0x18, 0x63, 0x01)
	unknown[0] = 0xae

	for name, raw := range map[string][]byte{
		"noncanonical integer": noncanonical,
		"duplicate key":        duplicate,
		"unknown key":          unknown,
	} {
		t.Run(name, func(t *testing.T) {
			bundle := valid
			bundle.AuthorizationToken = signedRawApproval(serviceApprovalDomain, keyID, raw, privateKey)
			if _, _, err := verifyServiceApprovalBundle(bundle, authority, now); err == nil {
				t.Fatal("invalid CBOR was accepted")
			}
		})
	}
}

func TestMismatchedBundleDoesNotConsumePendingNonce(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller := testServiceController(t, runner, now)
	first, err := controller.prepare(t.Context(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := controller.prepare(t.Context(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	firstBundle := approveTestChallenge(t, first.Challenge, now)
	secondBundle := approveTestChallenge(t, second.Challenge, now)
	mismatched := ServiceApprovalBundle{
		AuthorizationToken: firstBundle.AuthorizationToken,
		AuditAttestation:   secondBundle.AuditAttestation,
	}
	if _, _, err := controller.consumeApproval(mismatched); err == nil {
		t.Fatal("individually valid but mismatched bundle was accepted")
	}
	if len(controller.pending) != 2 {
		t.Fatalf("mismatched bundle consumed a pending nonce; pending=%d", len(controller.pending))
	}
	if _, _, err := controller.consumeApproval(firstBundle); err != nil {
		t.Fatalf("correct bundle failed after mismatch: %v", err)
	}
}

func TestApprovalBindsPendingActionAndState(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller := testServiceController(t, runner, now)
	prepared, err := controller.prepare(t.Context(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := decodeApprovalChallenge(prepared.Challenge, now)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*approvalClaims){
		"action": func(c *approvalClaims) { c.Action = actionStop },
		"state":  func(c *approvalClaims) { c.BeforeHash = strings.Repeat("x", 43) },
	} {
		t.Run(name, func(t *testing.T) {
			forged := envelope.Claims
			mutate(&forged)
			bundle := signedBundleForClaims(t, forged, testApproverPrivateKey())
			if _, _, err := controller.consumeApproval(bundle); err == nil {
				t.Fatal("approval not matching the prepared action was accepted")
			}
			if len(controller.pending) != 1 {
				t.Fatal("wrong action or state consumed the pending nonce")
			}
		})
	}
}

func TestControlApprovalEvidenceRejectsTampering(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	_, _, authority, _, bundle := approvalFixture(t, now)
	_, evidence, err := verifyServiceApprovalBundle(bundle, authority, now)
	if err != nil {
		t.Fatal(err)
	}
	clone := func() ControlApprovalEvidence {
		out := evidence
		out.ClaimsCBOR = bytes.Clone(evidence.ClaimsCBOR)
		out.Proof = bytes.Clone(evidence.Proof)
		return out
	}
	mutations := map[string]func(*ControlApprovalEvidence){
		"domain":      func(e *ControlApprovalEvidence) { e.Domain = serviceApprovalDomain },
		"claims hash": func(e *ControlApprovalEvidence) { e.AuthorizationClaimsSHA256[0] ^= 1 },
		"action ID":   func(e *ControlApprovalEvidence) { e.ActionID = "other-action" },
		"nonce":       func(e *ControlApprovalEvidence) { e.NonceSHA256[0] ^= 1 },
		"expiry":      func(e *ControlApprovalEvidence) { e.ExpiresAtUnix++ },
		"key ID":      func(e *ControlApprovalEvidence) { e.ApproverKeyID = "other-key" },
		"claims":      func(e *ControlApprovalEvidence) { e.ClaimsCBOR[len(e.ClaimsCBOR)-1] ^= 1 },
		"proof":       func(e *ControlApprovalEvidence) { e.Proof[0] ^= 1 },
		"evidence hash": func(e *ControlApprovalEvidence) {
			e.EvidenceSHA256[0] ^= 1
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			tampered := clone()
			mutate(&tampered)
			if _, err := VerifyControlApprovalEvidence(tampered, authority.publicKeys); err == nil {
				t.Fatal("tampered evidence was accepted")
			}
		})
	}
	if _, err := VerifyControlApprovalEvidence(evidence, map[string]ed25519.PublicKey{}); err == nil {
		t.Fatal("evidence signed by an unconfigured key was accepted")
	}
}

func TestLoadApproverPublicKeysRejectsUnsafeDirectory(t *testing.T) {
	base := secureTempDir(t)
	makeDir := func(t *testing.T, mode os.FileMode, files map[string][]byte) string {
		t.Helper()
		dir := filepath.Join(base, strings.ReplaceAll(t.Name(), "/", "-"))
		if err := os.Mkdir(dir, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		for name, data := range files {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, data, 0o440); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o440); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	publicKey := testApproverPrivateKey().Public().(ed25519.PublicKey)

	t.Run("valid", func(t *testing.T) {
		dir := makeDir(t, 0o750, map[string][]byte{"operator.pub": publicKey})
		keys, err := loadApproverPublicKeysOwnedBy(dir, uint32(os.Getuid()))
		if err != nil || len(keys) != 1 {
			t.Fatalf("keys=%v error=%v", keys, err)
		}
	})
	t.Run("directory permissions", func(t *testing.T) {
		dir := makeDir(t, 0o770, map[string][]byte{"operator.pub": publicKey})
		if _, err := loadApproverPublicKeysOwnedBy(dir, uint32(os.Getuid())); err == nil {
			t.Fatal("writable key directory was accepted")
		}
	})
	t.Run("file permissions", func(t *testing.T) {
		dir := makeDir(t, 0o750, map[string][]byte{"operator.pub": publicKey})
		if err := os.Chmod(filepath.Join(dir, "operator.pub"), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := loadApproverPublicKeysOwnedBy(dir, uint32(os.Getuid())); err == nil {
			t.Fatal("writable public key was accepted")
		}
	})
	t.Run("duplicate key", func(t *testing.T) {
		dir := makeDir(t, 0o750, map[string][]byte{"one.pub": publicKey, "two.pub": publicKey})
		if _, err := loadApproverPublicKeysOwnedBy(dir, uint32(os.Getuid())); err == nil {
			t.Fatal("duplicate public keys were accepted")
		}
	})
	t.Run("unexpected entry", func(t *testing.T) {
		dir := makeDir(t, 0o750, map[string][]byte{"README": []byte("no")})
		if _, err := loadApproverPublicKeysOwnedBy(dir, uint32(os.Getuid())); err == nil {
			t.Fatal("unexpected directory entry was accepted")
		}
	})
	t.Run("symlink file", func(t *testing.T) {
		targetDir := makeDir(t, 0o750, map[string][]byte{"real.pub": publicKey})
		link := filepath.Join(targetDir, "link.pub")
		if err := os.Symlink(filepath.Join(targetDir, "real.pub"), link); err != nil {
			t.Fatal(err)
		}
		if _, err := loadApproverPublicKeysOwnedBy(targetDir, uint32(os.Getuid())); err == nil {
			t.Fatal("symlink public key was accepted")
		}
	})
	t.Run("symlink directory", func(t *testing.T) {
		targetDir := makeDir(t, 0o750, map[string][]byte{"operator.pub": publicKey})
		link := filepath.Join(base, "approvers-link")
		if err := os.Symlink(targetDir, link); err != nil {
			t.Fatal(err)
		}
		if _, err := loadApproverPublicKeysOwnedBy(link, uint32(os.Getuid())); err == nil {
			t.Fatal("symlink key directory was accepted")
		}
	})
	t.Run("symlink ancestor", func(t *testing.T) {
		realParent := filepath.Join(base, "approvers-real-parent")
		if err := os.Mkdir(realParent, 0o750); err != nil {
			t.Fatal(err)
		}
		targetDir := filepath.Join(realParent, "keys")
		if err := os.Mkdir(targetDir, 0o750); err != nil {
			t.Fatal(err)
		}
		keyPath := filepath.Join(targetDir, "operator.pub")
		if err := os.WriteFile(keyPath, publicKey, 0o440); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "approvers-parent-link")
		if err := os.Symlink(realParent, link); err != nil {
			t.Fatal(err)
		}
		if _, err := loadApproverPublicKeysOwnedBy(
			filepath.Join(link, "keys"),
			uint32(os.Getuid()),
		); err == nil {
			t.Fatal("key directory beneath a symlinked ancestor was accepted")
		}
	})
	if os.Getuid() != 0 {
		t.Run("wrong owner", func(t *testing.T) {
			dir := makeDir(t, 0o750, map[string][]byte{"operator.pub": publicKey})
			if _, err := loadApproverPublicKeysOwnedBy(dir, uint32(os.Getuid()+1)); err == nil {
				t.Fatal("key directory owned by another user was accepted")
			}
		})
	}
}

func TestLoadApproverPrivateKeyRejectsSymlinkAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("trusted file reads intentionally fail closed on Windows")
	}
	base := secureTempDir(t)
	realParent := filepath.Join(base, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(realParent, "approver.key")
	if err := os.WriteFile(path, testApproverPrivateKey().Seed(), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "parent-link")
	if err := os.Symlink(realParent, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApproverPrivateKey(filepath.Join(link, "approver.key")); err == nil {
		t.Fatal("private key beneath a symlinked ancestor was accepted")
	}
}
