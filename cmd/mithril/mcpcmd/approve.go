package mcpcmd

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Overclock-Validator/mithril/internal/safedisplay"
	"github.com/Overclock-Validator/mithril/internal/safefile"
	"github.com/Overclock-Validator/mithril/pkg/mcp"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var approverPrivateKeyFile string

type serviceApprovalInspector func(challenge string, now time.Time) (mcp.ServiceApprovalSummary, error)
type serviceApprovalSigner func(challenge, keyPath string, now time.Time) (mcp.ServiceApprovalBundle, error)

var approveCmd = cobra.Command{
	Use:           "approve [CHALLENGE]",
	Short:         "Approve one prepared Mithril service action",
	SilenceErrors: true,
	SilenceUsage:  true,
	Long: `Verify a prepared service-action challenge and approve it interactively.
The prompt shows the target, action ID, current service state, exact action,
consequences, approver key ID, and expiry. Type APPROVE to sign. The private
key is opened only after confirmation. Prompts go to stderr; stdout contains
only the JSON approval bundle.

Run this command on the separate approver host. The MCP server needs only the
matching public key and must never receive the private key.

Omit CHALLENGE to paste it at the terminal prompt. Passing it as an argument is
supported for compatibility, but may expose it to process inspection or shell
history.`,
	Example: `  mithril mcp approve \
    --approval-key-file /absolute/path/to/approver.seed

  # Compatibility form; less private because the challenge is in argv:
  mithril mcp approve CHALLENGE \
    --approval-key-file /absolute/path/to/approver.seed`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		challenge := ""
		if len(args) == 1 {
			challenge = args[0]
		}
		return runApproveCommand(
			os.Stdin,
			cmd.OutOrStdout(),
			cmd.ErrOrStderr(),
			challenge,
			approverPrivateKeyFile,
			term.IsTerminal(int(os.Stdin.Fd())),
			time.Now,
			mcp.InspectServiceChallenge,
			signServiceApproval,
		)
	},
}

var initApprovalKeyCmd = cobra.Command{
	Use:           "init-approval-key PRIVATE_KEY [PUBLIC_KEY]",
	Short:         "Create a service-approval Ed25519 key pair",
	SilenceErrors: true,
	SilenceUsage:  true,
	Long: `Create a raw 32-byte Ed25519 seed with mode 0600 and a raw 32-byte
public key with mode 0440. Both paths must be absolute and neither file may
belong to another key pair. When PUBLIC_KEY is omitted it defaults to
PRIVATE_KEY + ".pub". A valid existing private seed is reused to finish an
interrupted public-key creation.
The private key's parent directory must be accessible only to its owner. Path
ancestors must be real directories, not symbolic links.

Keep the private seed on the separate approver host. Install only the public
key in the MCP server's root-managed approver directory.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		publicPath := args[0] + ".pub"
		if len(args) == 2 {
			publicPath = args[1]
		}
		keyID, err := createApprovalKeyPair(args[0], publicPath, rand.Reader)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(
			cmd.OutOrStdout(),
			"Approver private key ready: %s\nApprover public key ready: %s\nApprover key ID: %s\n",
			args[0],
			publicPath,
			keyID,
		)
		return err
	},
}

type approvalKeyDestination struct {
	path                 string
	parentPath           string
	name                 string
	parentInfo           os.FileInfo
	forbiddenPermissions os.FileMode
	root                 *os.Root
}

var syncApprovalKeyDirectory = func(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

var syncApprovalKeyFile = func(file *os.File) error {
	return file.Sync()
}

var readApprovalChallengeWithoutEcho = func(fd int) ([]byte, error) {
	return term.ReadPassword(fd)
}

func createApprovalKeyPair(privatePath, publicPath string, random io.Reader) (string, error) {
	for _, path := range []string{privatePath, publicPath} {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return "", errors.New("approval key paths must be clean absolute paths")
		}
	}
	if privatePath == publicPath {
		return "", errors.New("approval private and public key paths must differ")
	}

	privateDestination, err := openApprovalKeyDestination(privatePath, true)
	if err != nil {
		return "", fmt.Errorf("prepare private approval key: %w", err)
	}
	defer privateDestination.root.Close()
	publicDestination, err := openApprovalKeyDestination(publicPath, false)
	if err != nil {
		return "", fmt.Errorf("prepare public approval key: %w", err)
	}
	defer publicDestination.root.Close()

	privateExists, err := privateDestination.exists()
	if err != nil {
		return "", err
	}
	publicExists, err := publicDestination.exists()
	if err != nil {
		return "", err
	}
	if !privateExists && publicExists {
		return "", errors.New("public approval key already exists without its private key")
	}

	var seed []byte
	if privateExists {
		seed, err = readExistingApprovalKey(privateDestination, ed25519.SeedSize, 0o077, 0)
		if err != nil {
			return "", fmt.Errorf("load existing private approval key: %w", err)
		}
	} else {
		seed = make([]byte, ed25519.SeedSize)
		if _, err := io.ReadFull(random, seed); err != nil {
			clear(seed)
			return "", errors.New("failed to generate approval key")
		}
	}
	defer clear(seed)

	privateKey := ed25519.NewKeyFromSeed(seed)
	defer clear(privateKey)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	keyID, err := mcp.ApproverKeyID(publicKey)
	if err != nil {
		return "", err
	}
	if !privateExists {
		if err := writeApprovalKeyFile(privateDestination, seed, 0o600); err != nil {
			return "", fmt.Errorf("create private approval key: %w", err)
		}
	}
	if publicExists {
		existingPublic, err := readExistingApprovalKey(
			publicDestination,
			ed25519.PublicKeySize,
			0o022,
			0o440,
		)
		if err != nil {
			return "", fmt.Errorf("load existing public approval key: %w", err)
		}
		defer clear(existingPublic)
		if !ed25519.PublicKey(existingPublic).Equal(publicKey) {
			return "", errors.New("existing public approval key does not match the private key")
		}
	} else if err := writeApprovalKeyFile(publicDestination, publicKey, 0o440); err != nil {
		return "", fmt.Errorf("create public approval key (private key remains in place): %w", err)
	}
	return keyID, nil
}

func prepareApprovalKeyDestination(path string, privateDirectory bool) (*approvalKeyDestination, error) {
	destination, err := openApprovalKeyDestination(path, privateDirectory)
	if err != nil {
		return nil, err
	}
	exists, err := destination.exists()
	if err != nil {
		destination.root.Close()
		return nil, err
	}
	if exists {
		destination.root.Close()
		return nil, errors.New("approval key already exists")
	}
	return destination, nil
}

func openApprovalKeyDestination(path string, privateDirectory bool) (*approvalKeyDestination, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("approval key path must be a clean absolute path")
	}
	if err := safefile.ValidateNoSymlinkAncestors(path); err != nil {
		return nil, fmt.Errorf("approval key path ancestors are unsafe: %w", err)
	}

	parentPath := filepath.Dir(path)
	parentInfo, err := os.Lstat(parentPath)
	if err != nil || !parentInfo.IsDir() {
		return nil, errors.New("approval key parent directory is unavailable")
	}
	if !safefile.OwnerTrusted(parentInfo) {
		return nil, errors.New("approval key parent directory has an untrusted owner")
	}
	forbiddenPermissions := os.FileMode(0o022)
	if privateDirectory {
		forbiddenPermissions = 0o077
	}
	if parentInfo.Mode().Perm()&forbiddenPermissions != 0 {
		if privateDirectory {
			return nil, errors.New("private approval key parent directory must not grant group or other access")
		}
		return nil, errors.New("public approval key parent directory must not be group or other writable")
	}

	root, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, errors.New("open approval key parent directory")
	}
	destination := &approvalKeyDestination{
		path:                 path,
		parentPath:           parentPath,
		name:                 filepath.Base(path),
		parentInfo:           parentInfo,
		forbiddenPermissions: forbiddenPermissions,
		root:                 root,
	}
	if err := destination.validateParentIdentity(); err != nil {
		root.Close()
		return nil, err
	}
	return destination, nil
}

func (destination *approvalKeyDestination) exists() (bool, error) {
	if err := destination.validateParentIdentity(); err != nil {
		return false, err
	}
	if _, err := destination.root.Lstat(destination.name); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, errors.New("approval key destination is unavailable")
}

func (destination *approvalKeyDestination) validateParentIdentity() error {
	if err := safefile.ValidateNoSymlinkAncestors(destination.path); err != nil {
		return fmt.Errorf("approval key path ancestors changed: %w", err)
	}
	pathInfo, err := os.Lstat(destination.parentPath)
	if err != nil || !pathInfo.IsDir() {
		return errors.New("approval key parent directory changed")
	}
	rootInfo, err := destination.root.Stat(".")
	if err != nil || !os.SameFile(destination.parentInfo, pathInfo) ||
		!os.SameFile(destination.parentInfo, rootInfo) {
		return errors.New("approval key parent directory changed")
	}
	if !safefile.OwnerTrusted(pathInfo) || !safefile.OwnerTrusted(rootInfo) ||
		pathInfo.Mode().Perm()&destination.forbiddenPermissions != 0 ||
		rootInfo.Mode().Perm()&destination.forbiddenPermissions != 0 {
		return errors.New("approval key parent directory became unsafe")
	}
	return nil
}

func readExistingApprovalKey(
	destination *approvalKeyDestination,
	size int,
	forbiddenPermissions os.FileMode,
	requiredPermissions os.FileMode,
) ([]byte, error) {
	if err := destination.validateParentIdentity(); err != nil {
		return nil, err
	}
	validatePermissions := func() error {
		if requiredPermissions == 0 {
			return nil
		}
		info, err := destination.root.Lstat(destination.name)
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("existing approval key is not a trusted key file")
		}
		if info.Mode().Perm() != requiredPermissions {
			return fmt.Errorf(
				"existing approval key permissions must be %04o",
				requiredPermissions.Perm(),
			)
		}
		return nil
	}
	if err := validatePermissions(); err != nil {
		return nil, err
	}
	data, err := safefile.ReadTrustedRegular(destination.path, safefile.ReadOptions{
		MaxBytes:               int64(size),
		ForbiddenPerm:          forbiddenPermissions,
		RejectAncestorSymlinks: true,
	})
	if err != nil || len(data) != size {
		clear(data)
		return nil, errors.New("existing approval key is not a trusted key file")
	}
	if err := destination.validateParentIdentity(); err != nil {
		clear(data)
		return nil, err
	}
	if err := validatePermissions(); err != nil {
		clear(data)
		return nil, err
	}
	return data, nil
}

func writeApprovalKeyFile(destination *approvalKeyDestination, data []byte, mode os.FileMode) error {
	if err := destination.validateParentIdentity(); err != nil {
		return err
	}
	exists, err := destination.exists()
	if err != nil {
		return err
	}
	if exists {
		return errors.New("approval key already exists")
	}

	tempName, file, err := createApprovalKeyTemp(destination.root)
	if err != nil {
		return err
	}
	tempRemoved := false
	createdInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return errors.New("inspect approval key staging file")
	}
	defer func() {
		_ = file.Close()
		if !tempRemoved {
			if removeKnownApprovalKeyTemp(destination.root, tempName, createdInfo) {
				_ = syncApprovalKeyDirectory(destination.root)
			}
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return errors.New("set approval key permissions")
	}
	written, err := file.Write(data)
	if err != nil || written != len(data) {
		return errors.New("write approval key")
	}
	if err := syncApprovalKeyFile(file); err != nil {
		return errors.New("sync approval key")
	}
	verifiedInfo, err := file.Stat()
	if err != nil || !os.SameFile(createdInfo, verifiedInfo) ||
		!verifiedInfo.Mode().IsRegular() ||
		verifiedInfo.Mode().Perm() != mode || verifiedInfo.Size() != int64(len(data)) {
		return errors.New("verify approval key")
	}
	if err := destination.root.Link(tempName, destination.name); err != nil {
		return fmt.Errorf("publish approval key: %w", err)
	}
	pathInfo, err := destination.root.Lstat(destination.name)
	if err != nil || !os.SameFile(createdInfo, pathInfo) {
		return errors.New("verify published approval key")
	}
	if !removeKnownApprovalKeyTemp(destination.root, tempName, createdInfo) {
		return errors.New("remove approval key staging file")
	}
	tempRemoved = true
	if err := syncApprovalKeyDirectory(destination.root); err != nil {
		return errors.New("sync approval key directory")
	}
	if err := file.Close(); err != nil {
		return errors.New("close approval key")
	}
	if err := destination.validateParentIdentity(); err != nil {
		return err
	}
	pathInfo, err = destination.root.Lstat(destination.name)
	if err != nil || !os.SameFile(createdInfo, pathInfo) ||
		!pathInfo.Mode().IsRegular() || pathInfo.Mode().Perm() != mode ||
		pathInfo.Size() != int64(len(data)) {
		return errors.New("approval key path changed during creation")
	}
	return nil
}

func createApprovalKeyTemp(root *os.Root) (string, *os.File, error) {
	for range 16 {
		var nonce [16]byte
		if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
			return "", nil, errors.New("create approval key staging name")
		}
		name := ".mithril-approval-" + hex.EncodeToString(nonce[:]) + ".tmp"
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return name, file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, errors.New("create approval key staging file")
		}
	}
	return "", nil, errors.New("create unique approval key staging file")
}

func removeKnownApprovalKeyTemp(root *os.Root, name string, createdInfo os.FileInfo) bool {
	if name == "" || createdInfo == nil {
		return false
	}
	pathInfo, err := root.Lstat(name)
	if err != nil || !os.SameFile(createdInfo, pathInfo) {
		return false
	}
	return root.Remove(name) == nil
}

func runApproveCommand(
	in io.Reader,
	stdout, stderr io.Writer,
	challenge, keyFlag string,
	stdinIsTerminal bool,
	now func() time.Time,
	inspect serviceApprovalInspector,
	sign serviceApprovalSigner,
) error {
	if !stdinIsTerminal {
		return errors.New("service approval requires an interactive terminal")
	}
	keyPath, err := resolveApprovalKeyPath(keyFlag)
	if err != nil {
		return err
	}
	input := bufio.NewReaderSize(in, maxApprovalChallengeInputBytes+2)
	if challenge == "" {
		challenge, err = readServiceApprovalChallenge(in, input, stderr)
		if err != nil {
			return err
		}
	}
	summary, err := inspect(challenge, now())
	if err != nil {
		return err
	}
	if err := confirmServiceApproval(input, stderr, summary); err != nil {
		return err
	}
	bundle, err := sign(challenge, keyPath, now())
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(bundle); err != nil {
		return errors.New("failed to write approval bundle")
	}
	return nil
}

func resolveApprovalKeyPath(flagValue string) (string, error) {
	keyPath := flagValue
	if keyPath == "" {
		keyPath = os.Getenv("MITHRIL_MCP_APPROVER_PRIVATE_KEY_FILE")
	}
	if keyPath == "" || !filepath.IsAbs(keyPath) || filepath.Clean(keyPath) != keyPath {
		return "", errors.New("--approval-key-file or MITHRIL_MCP_APPROVER_PRIVATE_KEY_FILE must be a clean absolute path")
	}
	return keyPath, nil
}

func signServiceApproval(challenge, keyPath string, now time.Time) (mcp.ServiceApprovalBundle, error) {
	privateKey, err := mcp.LoadApproverPrivateKey(keyPath)
	if err != nil {
		return mcp.ServiceApprovalBundle{}, err
	}
	defer clear(privateKey)
	return mcp.ApproveServiceChallenge(challenge, privateKey, now)
}

const (
	maxApprovalChallengeInputBytes = 8 * 1024
	maxApprovalConfirmationBytes   = 64
)

func readServiceApprovalChallenge(terminalInput io.Reader, bufferedInput *bufio.Reader, stderr io.Writer) (string, error) {
	if _, err := fmt.Fprint(stderr, "Paste the prepared service-action challenge, then press Enter: "); err != nil {
		return "", errors.New("failed to write approval challenge prompt")
	}
	var challenge string
	if terminal, ok := terminalInput.(interface{ Fd() uintptr }); ok {
		encoded, err := readApprovalChallengeWithoutEcho(int(terminal.Fd()))
		_, newlineErr := fmt.Fprintln(stderr)
		if err != nil || newlineErr != nil || len(encoded) > maxApprovalChallengeInputBytes {
			clear(encoded)
			return "", errors.New("failed to read a bounded approval challenge")
		}
		challenge = string(encoded)
		clear(encoded)
	} else {
		var err error
		challenge, err = readBoundedApprovalLine(bufferedInput, maxApprovalChallengeInputBytes)
		if err != nil {
			return "", errors.New("failed to read a bounded approval challenge")
		}
	}
	if challenge == "" {
		return "", errors.New("approval challenge is required")
	}
	return challenge, nil
}

func readBoundedApprovalLine(in io.Reader, maxBytes int) (string, error) {
	reader := bufio.NewReaderSize(in, maxApprovalChallengeInputBytes+2)
	line, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return "", errors.New("approval input is too long")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", errors.New("failed to read approval input")
	}
	line = bytesTrimLineEnding(line)
	if len(line) > maxBytes {
		return "", errors.New("approval input is too long")
	}
	return string(line), nil
}

func bytesTrimLineEnding(line []byte) []byte {
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return line
}

func confirmServiceApproval(in io.Reader, stderr io.Writer, summary mcp.ServiceApprovalSummary) error {
	loadState := safedisplay.Text(summary.Status.LoadState, nil)
	activeState := safedisplay.Text(summary.Status.ActiveState, nil)
	subState := safedisplay.Text(summary.Status.SubState, nil)
	if _, err := fmt.Fprintf(stderr,
		"Mithril service action approval\n\nTarget: %s\nAction ID: %s\nAction: %s\nUnit: %s\nScope: %s\nCurrent state: %s/%s/%s (PID %d, restarts %d)\nConsequence: %s\nApprover key ID: %s\nExpires: %s\n\nType APPROVE to authorize this exact action: ",
		summary.TargetID,
		summary.ActionID,
		summary.Action,
		summary.Unit,
		summary.Scope,
		loadState,
		activeState,
		subState,
		summary.Status.MainPID,
		summary.Status.NRestarts,
		summary.Consequence,
		summary.ApproverKeyID,
		time.Unix(summary.ExpiresAt, 0).UTC().Format(time.RFC3339),
	); err != nil {
		return errors.New("failed to write approval prompt")
	}

	confirmation, err := readBoundedApprovalLine(in, maxApprovalConfirmationBytes)
	if err != nil {
		return errors.New("service action was not approved")
	}
	if confirmation != "APPROVE" {
		return errors.New("service action was not approved")
	}
	return nil
}
