package main

import (
	"context"
	"flag"
	"os"
	"os/signal"

	"github.com/Overclock-Validator/mithril/cmd/mithril/configcmd"
	"github.com/Overclock-Validator/mithril/cmd/mithril/node"
	"github.com/Overclock-Validator/mithril/pkg/config"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"

	// Load in instruction pretty-printing
	_ "github.com/gagliardetto/solana-go/programs/system"
	_ "github.com/gagliardetto/solana-go/programs/vote"
)

var cmd = cobra.Command{
	Use:   "mithril",
	Short: "mithril Solana verifier node",
	Long: `Mithril is a lightweight Solana verifier node that replays and validates transactions.

Quick start:
  1. mithril config init              # Generate config.toml
  2. Edit config.toml                 # Set storage paths
  3. mithril run --config config.toml # Start Mithril

Disk setup (recommended before first run):
  sudo ./scripts/disk-setup.sh --setup    # Format and mount NVMe drives
  ./scripts/disk-setup.sh --status        # Show current storage status
  ./scripts/disk-setup.sh                 # Show all disk commands`,
}

func init() {
	klogFlags := flag.NewFlagSet("klog", flag.ExitOnError)
	klog.InitFlags(klogFlags)
	cmd.PersistentFlags().AddGoFlagSet(klogFlags)

	// Add config file flag
	cmd.PersistentFlags().StringVar(&config.ConfigFile, "config", "", "Path to TOML config file")

	cmd.AddCommand(
		&node.Run,              // Primary command for running Mithril
		&configcmd.ConfigCmd,   // Config management (init, etc.)
		&node.VerifyRange,      // Developer/advanced command
		&node.VerifyLive,       // Backwards compatibility alias for Run
	)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	cobra.CheckErr(cmd.ExecuteContext(ctx))
}
