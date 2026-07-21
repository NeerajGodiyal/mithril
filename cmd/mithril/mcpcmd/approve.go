package mcpcmd

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/mcp"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var approvalKeyFile string

type serviceApproval struct {
	Action    string
	Unit      string
	Scope     string
	ExpiresAt string
	Token     string
}

type serviceApprovalLoader func(challenge, keyPath string, now time.Time) (serviceApproval, error)

var approveCmd = cobra.Command{
	Use:           "approve CHALLENGE",
	Short:         "Approve one prepared Mithril service action",
	SilenceErrors: true,
	SilenceUsage:  true,
	Long: `Verify a prepared service-action challenge and approve it interactively.
The prompt shows the exact action, systemd unit, scope, and expiry. Type APPROVE
to produce a short-lived token. Prompts go to stderr; stdout contains only the
approved token. This command requires a real terminal and has no noninteractive
flag.

For an SSH-backed MCP session, run this command on the MCP server host so the
approval key stays there, using the same OS user that launched MCP. Then return
the token to the same MCP session.`,
	Example: `  mithril mcp approve CHALLENGE --approval-key-file /absolute/path/to/approval.key

  ssh -t NODE /absolute/path/to/mithril mcp approve CHALLENGE \
    --approval-key-file /absolute/path/to/approval.key`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runApproveCommand(
			os.Stdin,
			cmd.OutOrStdout(),
			cmd.ErrOrStderr(),
			args[0],
			approvalKeyFile,
			term.IsTerminal(int(os.Stdin.Fd())),
			time.Now(),
			loadServiceApproval,
		)
	},
}

var initApprovalKeyCmd = cobra.Command{
	Use:           "init-approval-key FILE",
	Short:         "Create a service-approval key",
	SilenceErrors: true,
	SilenceUsage:  true,
	Long: `Create a new 32-byte cryptographic key at an absolute path with mode
0600. The command refuses to replace an existing file. Run it on the host where
the operator-profile MCP server will run, as the same OS user that will launch
that server.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := createApprovalKey(args[0], rand.Reader); err != nil {
			return err
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Created MCP approval key: %s\n", args[0])
		return err
	},
}

func createApprovalKey(path string, random io.Reader) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("approval key path must be a clean absolute path")
	}
	key := make([]byte, 32)
	defer clear(key)
	if _, err := io.ReadFull(random, key); err != nil {
		return errors.New("failed to generate approval key")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create approval key: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return errors.New("set approval key permissions")
	}
	if _, err := f.Write(key); err != nil {
		return errors.New("write approval key")
	}
	if err := f.Sync(); err != nil {
		return errors.New("sync approval key")
	}
	if err := f.Close(); err != nil {
		return errors.New("close approval key")
	}
	complete = true
	return nil
}

func runApproveCommand(in io.Reader, stdout, stderr io.Writer, challenge, keyFlag string, stdinIsTerminal bool, now time.Time, load serviceApprovalLoader) error {
	if !stdinIsTerminal {
		return errors.New("service approval requires an interactive terminal")
	}
	keyPath, err := resolveApprovalKeyPath(keyFlag)
	if err != nil {
		return err
	}
	approval, err := load(challenge, keyPath, now)
	if err != nil {
		return err
	}
	return confirmServiceApproval(in, stdout, stderr, approval)
}

func resolveApprovalKeyPath(flagValue string) (string, error) {
	keyPath := flagValue
	if keyPath == "" {
		keyPath = os.Getenv("MITHRIL_MCP_APPROVAL_KEY_FILE")
	}
	if keyPath == "" || !filepath.IsAbs(keyPath) || filepath.Clean(keyPath) != keyPath {
		return "", errors.New("--approval-key-file or MITHRIL_MCP_APPROVAL_KEY_FILE must be a clean absolute path")
	}
	return keyPath, nil
}

func loadServiceApproval(challenge, keyPath string, now time.Time) (serviceApproval, error) {
	key, err := mcp.LoadApprovalKey(keyPath)
	if err != nil {
		return serviceApproval{}, err
	}
	defer clear(key)
	claims, token, err := mcp.ApproveServiceChallenge(challenge, key, now)
	if err != nil {
		return serviceApproval{}, err
	}
	return serviceApproval{
		Action:    claims.Action,
		Unit:      claims.Unit,
		Scope:     claims.Scope,
		ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC().Format(time.RFC3339),
		Token:     token,
	}, nil
}

const maxApprovalConfirmationBytes = 64

func confirmServiceApproval(in io.Reader, stdout, stderr io.Writer, approval serviceApproval) error {
	if _, err := fmt.Fprintf(stderr,
		"Mithril service action approval\n\nAction: %s\nUnit: %s\nScope: %s\nExpires: %s\n\nType APPROVE to authorize this exact action: ",
		approval.Action, approval.Unit, approval.Scope, approval.ExpiresAt,
	); err != nil {
		return errors.New("failed to write approval prompt")
	}

	reader := bufio.NewReader(io.LimitReader(in, maxApprovalConfirmationBytes+1))
	confirmation, readErr := reader.ReadString('\n')
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return errors.New("failed to read approval confirmation")
	}
	if len(confirmation) > maxApprovalConfirmationBytes {
		return errors.New("service action was not approved")
	}
	confirmation = strings.TrimSuffix(confirmation, "\n")
	confirmation = strings.TrimSuffix(confirmation, "\r")
	if confirmation != "APPROVE" {
		return errors.New("service action was not approved")
	}
	if _, err := fmt.Fprintln(stdout, approval.Token); err != nil {
		return errors.New("failed to write approved token")
	}
	return nil
}
