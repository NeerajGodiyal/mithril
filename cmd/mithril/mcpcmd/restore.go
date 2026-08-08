package mcpcmd

import (
	"encoding/json"
	"errors"

	"github.com/Overclock-Validator/mithril/pkg/mcp"
	"github.com/spf13/cobra"
)

var restoreControl mcp.ControlRestoreConfig

var restoreControlState = mcp.RestoreControlState

var restoreControlCmd = cobra.Command{
	Use:           "restore-control",
	Short:         "Restore operator state from the off-host audit",
	Args:          cobra.NoArgs,
	SilenceErrors: true,
	SilenceUsage:  true,
	Long: `Restore operation.json after node-host loss.

First copy the receiver's private control-audit.jsonl into the control state
directory through the administrator recovery channel. This command verifies
the entire copied chain, compares it with the live pinned receiver summary,
and restores only a missing or byte-identical operation state. It never calls
systemctl and has no force or overwrite mode.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if restoreControl.ControlStateDir == "" ||
			restoreControl.ApproverKeysDir == "" ||
			restoreControl.AuditClientConfigPath == "" ||
			restoreControl.TargetID == "" ||
			restoreControl.SystemdUnit == "" ||
			restoreControl.SystemdScope == "" {
			return errors.New("restore-control requires the state directory, approver keys, audit client config, target ID, systemd unit, and systemd scope")
		}
		result, err := restoreControlState(cmd.Context(), restoreControl)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	},
}

func init() {
	flags := restoreControlCmd.Flags()
	flags.StringVar(
		&restoreControl.ControlStateDir,
		"control-state-dir",
		"",
		"private directory containing the copied control-audit.jsonl",
	)
	flags.StringVar(
		&restoreControl.ApproverKeysDir,
		"approver-keys-dir",
		"",
		"directory containing active approver public keys",
	)
	flags.StringVar(
		&restoreControl.ApproverHistoryKeysDir,
		"approver-history-keys-dir",
		"",
		"directory containing retained historical approver public keys; defaults to --approver-keys-dir",
	)
	flags.StringVar(
		&restoreControl.AuditClientConfigPath,
		"audit-client-config",
		"",
		"pinned mTLS audit client configuration",
	)
	flags.StringVar(
		&restoreControl.TargetID,
		"control-target-id",
		"",
		"expected operator control target ID",
	)
	flags.StringVar(
		&restoreControl.SystemdUnit,
		"systemd-unit",
		"",
		"expected fixed systemd service unit",
	)
	flags.StringVar(
		&restoreControl.SystemdScope,
		"systemd-scope",
		"",
		"expected systemd scope: system or user",
	)
	MCPCmd.AddCommand(&restoreControlCmd)
}
