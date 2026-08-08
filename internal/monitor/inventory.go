package monitor

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Overclock-Validator/mithril/internal/safefile"
)

const (
	inventorySchemaVersion = 1
	maxInventoryBytes      = 64 << 10
	maxPublicKeyBytes      = 4 << 10
	maxManifestIDBytes     = 64
	maxMountpointBytes     = 4096
)

const (
	TargetNode             = "mithril-node"
	TargetMonitor          = "mithril-monitor"
	TargetNotifier         = "mithril-notifier"
	TargetAgent            = "mithril-agent"
	TargetNodeExporter     = "node-exporter"
	TargetBlackboxExporter = "blackbox-exporter"
	TargetPrometheus       = "prometheus"
	TargetAlertmanager     = "alertmanager"

	FilesystemRoot     = "root"
	FilesystemAccounts = "accounts"
	FilesystemLedger   = "ledger"
)

var (
	expectedTargetJobs = []string{
		TargetNode,
		TargetMonitor,
		TargetNotifier,
		TargetAgent,
		TargetNodeExporter,
		TargetBlackboxExporter,
		TargetPrometheus,
		TargetAlertmanager,
	}
	expectedFilesystemRoles = []string{
		FilesystemRoot,
		FilesystemAccounts,
		FilesystemLedger,
	}
	manifestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// DeploymentManifest is the signed, bounded inventory used by the off-host
// monitor. It contains no endpoint or credential.
type DeploymentManifest struct {
	SchemaVersion int
	ManifestID    string
	Targets       []ExpectedTarget
	Filesystems   []ExpectedFilesystem
}

type ExpectedTarget struct {
	TargetJob string
	Required  bool
}

type ExpectedFilesystem struct {
	Role       string
	Mountpoint string
	Required   bool
}

type signedManifest struct {
	manifest DeploymentManifest
	digest   [sha256.Size]byte
}

type manifestWire struct {
	SchemaVersion int              `json:"schema_version"`
	ManifestID    string           `json:"manifest_id"`
	Targets       []targetWire     `json:"targets"`
	Filesystems   []filesystemWire `json:"filesystems"`
}

type targetWire struct {
	TargetJob string `json:"target_job"`
	Required  *bool  `json:"required"`
}

type filesystemWire struct {
	Role       string `json:"role"`
	Mountpoint string `json:"mountpoint"`
	Required   *bool  `json:"required"`
}

// loadSignedManifest verifies the detached signature over the raw manifest
// bytes before interpreting any JSON. The private signing key never belongs on
// the monitor host.
func loadSignedManifest(manifestPath, signaturePath, publicKeyPath string) (signedManifest, error) {
	raw, err := readTrustedFile(manifestPath, "inventory manifest", maxInventoryBytes)
	if err != nil {
		return signedManifest{}, err
	}
	signature, err := readTrustedFile(signaturePath, "inventory signature", ed25519.SignatureSize)
	if err != nil {
		return signedManifest{}, err
	}
	if len(signature) != ed25519.SignatureSize {
		return signedManifest{}, errors.New("inventory signature has the wrong size")
	}
	publicPEM, err := readTrustedFile(publicKeyPath, "inventory public key", maxPublicKeyBytes)
	if err != nil {
		return signedManifest{}, err
	}
	publicKey, err := parseInventoryPublicKey(publicPEM)
	if err != nil {
		return signedManifest{}, err
	}
	if !ed25519.Verify(publicKey, raw, signature) {
		return signedManifest{}, errors.New("inventory signature verification failed")
	}

	manifest, err := decodeManifest(raw)
	if err != nil {
		return signedManifest{}, err
	}
	return signedManifest{
		manifest: manifest,
		digest:   sha256.Sum256(raw),
	}, nil
}

func parseInventoryPublicKey(raw []byte) (ed25519.PublicKey, error) {
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "PUBLIC KEY" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("inventory public key is malformed")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("inventory public key is malformed")
	}
	publicKey, ok := key.(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("inventory public key is not Ed25519")
	}
	return publicKey, nil
}

func decodeManifest(raw []byte) (DeploymentManifest, error) {
	if !utf8.Valid(raw) {
		return DeploymentManifest{}, errors.New("inventory manifest is not valid UTF-8")
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return DeploymentManifest{}, errors.New("inventory manifest is malformed")
	}

	var wire manifestWire
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return DeploymentManifest{}, errors.New("inventory manifest is malformed")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return DeploymentManifest{}, errors.New("inventory manifest is malformed")
	}
	return validateManifest(wire)
}

func validateManifest(wire manifestWire) (DeploymentManifest, error) {
	if wire.SchemaVersion != inventorySchemaVersion {
		return DeploymentManifest{}, errors.New("inventory manifest schema_version must be 1")
	}
	if len(wire.ManifestID) == 0 || len(wire.ManifestID) > maxManifestIDBytes ||
		!manifestIDPattern.MatchString(wire.ManifestID) {
		return DeploymentManifest{}, errors.New("inventory manifest_id is invalid")
	}

	targets, err := validateTargets(wire.Targets)
	if err != nil {
		return DeploymentManifest{}, err
	}
	filesystems, err := validateFilesystems(wire.Filesystems)
	if err != nil {
		return DeploymentManifest{}, err
	}
	return DeploymentManifest{
		SchemaVersion: wire.SchemaVersion,
		ManifestID:    wire.ManifestID,
		Targets:       targets,
		Filesystems:   filesystems,
	}, nil
}

func validateTargets(wire []targetWire) ([]ExpectedTarget, error) {
	if len(wire) != len(expectedTargetJobs) {
		return nil, errors.New("inventory manifest must contain exactly eight targets")
	}
	allowed := make(map[string]bool, len(expectedTargetJobs))
	for _, job := range expectedTargetJobs {
		allowed[job] = true
	}
	seen := make(map[string]bool, len(wire))
	targets := make([]ExpectedTarget, 0, len(wire))
	for _, target := range wire {
		if !allowed[target.TargetJob] || seen[target.TargetJob] {
			return nil, errors.New("inventory manifest target inventory is invalid")
		}
		if target.Required == nil {
			return nil, errors.New("inventory manifest target required field is missing")
		}
		if target.TargetJob != TargetAgent && !*target.Required {
			return nil, errors.New("only mithril-agent may be an optional target")
		}
		seen[target.TargetJob] = true
		targets = append(targets, ExpectedTarget{TargetJob: target.TargetJob, Required: *target.Required})
	}
	return targets, nil
}

func validateFilesystems(wire []filesystemWire) ([]ExpectedFilesystem, error) {
	if len(wire) != len(expectedFilesystemRoles) {
		return nil, errors.New("inventory manifest must contain exactly three filesystem roles")
	}
	allowed := make(map[string]bool, len(expectedFilesystemRoles))
	for _, role := range expectedFilesystemRoles {
		allowed[role] = true
	}
	seen := make(map[string]bool, len(wire))
	filesystems := make([]ExpectedFilesystem, 0, len(wire))
	for _, filesystem := range wire {
		if !allowed[filesystem.Role] || seen[filesystem.Role] {
			return nil, errors.New("inventory manifest filesystem inventory is invalid")
		}
		if filesystem.Required == nil || !*filesystem.Required {
			return nil, errors.New("inventory manifest filesystem roles must be required")
		}
		if !validMountpoint(filesystem.Mountpoint) {
			return nil, errors.New("inventory manifest filesystem mountpoint is invalid")
		}
		if filesystem.Role == FilesystemRoot && filesystem.Mountpoint != "/" {
			return nil, errors.New("inventory manifest root mountpoint must be /")
		}
		seen[filesystem.Role] = true
		filesystems = append(filesystems, ExpectedFilesystem{
			Role:       filesystem.Role,
			Mountpoint: filesystem.Mountpoint,
			Required:   true,
		})
	}
	return filesystems, nil
}

func validMountpoint(value string) bool {
	return value != "" &&
		len(value) <= maxMountpointBytes &&
		utf8.ValidString(value) &&
		strings.TrimSpace(value) == value &&
		filepath.IsAbs(value) &&
		filepath.Clean(value) == value
}

func readTrustedFile(path, kind string, maxBytes int64) ([]byte, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New(kind + " path must be a clean absolute path")
	}
	// The signed inventory, its signature, and the public key that checks both.
	// A symlinked ancestor directory redirects all three together, so the
	// substitute manifest arrives with a signature that verifies against the
	// substitute key — the signature check cannot notice.
	raw, err := safefile.ReadTrustedRegular(path, safefile.ReadOptions{
		MaxBytes:               maxBytes,
		ForbiddenPerm:          0o022,
		RejectAncestorSymlinks: true,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", kind, err)
	}
	return raw, nil
}

// rejectDuplicateJSONKeys rejects duplicate fields at every object depth.
// encoding/json otherwise silently keeps the last value.
func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := walkJSONValue(decoder); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func walkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("duplicate object key")
			}
			seen[key] = struct{}{}
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("unterminated object")
		}
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("unterminated array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("extra JSON value")
	}
	return err
}
