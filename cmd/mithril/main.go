package main

import (
	"context"
	"flag"
	"os"
	"os/signal"

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
}

func init() {
	klogFlags := flag.NewFlagSet("klog", flag.ExitOnError)
	klog.InitFlags(klogFlags)
	cmd.PersistentFlags().AddGoFlagSet(klogFlags)

	// Add config file flag
	cmd.PersistentFlags().StringVar(&config.ConfigFile, "config", "", "Path to TOML config file")

	cmd.AddCommand(
		&node.VerifyRange,
	)
	cmd.AddCommand(
		&node.VerifyLive,
	)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	cobra.CheckErr(cmd.ExecuteContext(ctx))
}
