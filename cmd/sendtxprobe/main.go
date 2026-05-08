package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/rpc"
)

const (
	defaultAirdropLamports  = 20_000_000
	defaultTransferLamports = 1_000_000
	defaultCluster          = "testnet"
)

type config struct {
	mithrilRPC        string
	cluster           string
	clusterRPC        string
	airdropLamports   uint64
	transferLamports  uint64
	skipPreflight     bool
	timeout           time.Duration
	pollInterval      time.Duration
	postAirdropSettle time.Duration
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	cfg, err := parseFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sendtxprobe: %v\n", err)
		os.Exit(2)
	}

	if err := run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "sendtxprobe failed: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() (config, error) {
	cfg := config{}

	flag.StringVar(&cfg.mithrilRPC, "mithril-rpc", "", "HTTP URL for the Mithril RPC endpoint to exercise")
	flag.StringVar(&cfg.cluster, "cluster", defaultCluster, "Public Solana cluster for funding/confirmation: testnet or devnet")
	flag.StringVar(&cfg.clusterRPC, "cluster-rpc", "", "Override the public cluster RPC URL used for airdrop and confirmation")
	flag.Uint64Var(&cfg.airdropLamports, "airdrop-lamports", defaultAirdropLamports, "Lamports to request from the public faucet")
	flag.Uint64Var(&cfg.transferLamports, "transfer-lamports", defaultTransferLamports, "Lamports to send through Mithril")
	flag.BoolVar(&cfg.skipPreflight, "skip-preflight", false, "Pass skipPreflight=true to Mithril sendTransaction")
	flag.DurationVar(&cfg.timeout, "timeout", 3*time.Minute, "Max time to wait for the airdrop and transfer confirmations")
	flag.DurationVar(&cfg.pollInterval, "poll-interval", 2*time.Second, "Polling interval for signature confirmation")
	flag.DurationVar(&cfg.postAirdropSettle, "post-airdrop-settle", 5*time.Second, "Extra wait after the airdrop confirms before submitting via Mithril")
	flag.Parse()

	if cfg.mithrilRPC == "" {
		return config{}, errors.New("must provide -mithril-rpc")
	}
	if cfg.transferLamports == 0 {
		return config{}, errors.New("-transfer-lamports must be greater than zero")
	}
	if cfg.airdropLamports <= cfg.transferLamports {
		return config{}, fmt.Errorf("-airdrop-lamports (%d) must be greater than -transfer-lamports (%d)", cfg.airdropLamports, cfg.transferLamports)
	}
	if cfg.timeout <= 0 {
		return config{}, errors.New("-timeout must be greater than zero")
	}
	if cfg.pollInterval <= 0 {
		return config{}, errors.New("-poll-interval must be greater than zero")
	}
	if cfg.clusterRPC == "" {
		clusterRPC, err := clusterRPCFor(cfg.cluster)
		if err != nil {
			return config{}, err
		}
		cfg.clusterRPC = clusterRPC
	}

	return cfg, nil
}

func run(ctx context.Context, cfg config) error {
	clusterClient := rpc.New(cfg.clusterRPC)
	defer clusterClient.Close()

	mithrilClient := rpc.New(cfg.mithrilRPC)
	defer mithrilClient.Close()

	sender := solana.NewWallet()
	recipient := solana.NewWallet()

	fmt.Printf("Mithril RPC: %s\n", cfg.mithrilRPC)
	fmt.Printf("Cluster RPC: %s\n", cfg.clusterRPC)
	fmt.Printf("Sender: %s\n", sender.PublicKey())
	fmt.Printf("Recipient: %s\n", recipient.PublicKey())

	fmt.Printf("Requesting airdrop of %d lamports for sender...\n", cfg.airdropLamports)
	airdropSig, err := clusterClient.RequestAirdrop(ctx, sender.PublicKey(), cfg.airdropLamports, rpc.CommitmentConfirmed)
	if err != nil {
		return formatAirdropError(cfg.cluster, cfg.clusterRPC, err)
	}
	fmt.Printf("Airdrop signature: %s\n", airdropSig)

	if err := waitForSignature(ctx, clusterClient, airdropSig, cfg.timeout, cfg.pollInterval, "airdrop"); err != nil {
		return err
	}

	senderBalance, err := clusterClient.GetBalance(ctx, sender.PublicKey(), rpc.CommitmentConfirmed)
	if err != nil {
		return fmt.Errorf("get sender balance from %s: %w", cfg.clusterRPC, err)
	}
	fmt.Printf("Sender confirmed balance: %d lamports\n", senderBalance.Value)
	if senderBalance.Value < cfg.transferLamports {
		return fmt.Errorf("sender balance %d is lower than transfer amount %d", senderBalance.Value, cfg.transferLamports)
	}

	if cfg.postAirdropSettle > 0 {
		fmt.Printf("Waiting %s for Mithril to catch up with the airdrop...\n", cfg.postAirdropSettle)
		timer := time.NewTimer(cfg.postAirdropSettle)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}

	latestBlockhash, err := mithrilClient.GetLatestBlockhash(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		return fmt.Errorf("getLatestBlockhash via Mithril %s: %w", cfg.mithrilRPC, err)
	}
	if latestBlockhash == nil || latestBlockhash.Value == nil {
		return errors.New("Mithril getLatestBlockhash returned no value")
	}
	fmt.Printf("Mithril latest blockhash: %s\n", latestBlockhash.Value.Blockhash)

	instruction, err := system.NewTransferInstruction(
		cfg.transferLamports,
		sender.PublicKey(),
		recipient.PublicKey(),
	).ValidateAndBuild()
	if err != nil {
		return fmt.Errorf("build transfer instruction: %w", err)
	}

	tx, err := solana.NewTransaction(
		[]solana.Instruction{instruction},
		latestBlockhash.Value.Blockhash,
		solana.TransactionPayer(sender.PublicKey()),
	)
	if err != nil {
		return fmt.Errorf("build transaction: %w", err)
	}

	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(sender.PublicKey()) {
			return &sender.PrivateKey
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("sign transaction: %w", err)
	}

	txSig, err := mithrilClient.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{
		SkipPreflight:       cfg.skipPreflight,
		PreflightCommitment: rpc.CommitmentConfirmed,
	})
	if err != nil {
		return fmt.Errorf("sendTransaction via Mithril %s: %w", cfg.mithrilRPC, err)
	}
	fmt.Printf("Submitted via Mithril sendTransaction: %s\n", txSig)

	if err := waitForSignature(ctx, clusterClient, txSig, cfg.timeout, cfg.pollInterval, "transfer"); err != nil {
		return err
	}

	recipientBalance, err := clusterClient.GetBalance(ctx, recipient.PublicKey(), rpc.CommitmentConfirmed)
	if err != nil {
		return fmt.Errorf("get recipient balance from %s: %w", cfg.clusterRPC, err)
	}
	fmt.Printf("Recipient confirmed balance: %d lamports\n", recipientBalance.Value)
	if recipientBalance.Value < cfg.transferLamports {
		return fmt.Errorf("recipient balance %d is lower than transfer amount %d", recipientBalance.Value, cfg.transferLamports)
	}

	fmt.Printf("sendTransaction flow succeeded through Mithril.\n")
	return nil
}

func clusterRPCFor(cluster string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(cluster)) {
	case "testnet":
		return rpc.TestNet_RPC, nil
	case "devnet":
		return rpc.DevNet_RPC, nil
	default:
		return "", fmt.Errorf("unsupported -cluster %q; expected testnet or devnet", cluster)
	}
}

func waitForSignature(
	ctx context.Context,
	client *rpc.Client,
	signature solana.Signature,
	timeout time.Duration,
	pollInterval time.Duration,
	label string,
) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		status, err := getSignatureStatus(waitCtx, client, signature)
		if err == nil {
			if status.Err != nil {
				return fmt.Errorf("%s signature %s failed on cluster: %v", label, signature, status.Err)
			}
			if signatureConfirmed(status) {
				fmt.Printf("%s signature confirmed on cluster: %s\n", label, signature)
				return nil
			}
		} else if !errors.Is(err, rpc.ErrNotFound) {
			return fmt.Errorf("getSignatureStatuses for %s signature %s: %w", label, signature, err)
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for %s signature %s: %w", label, signature, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func getSignatureStatus(ctx context.Context, client *rpc.Client, signature solana.Signature) (*rpc.SignatureStatusesResult, error) {
	statuses, err := client.GetSignatureStatuses(ctx, true, signature)
	if err != nil {
		return nil, err
	}
	if len(statuses.Value) == 0 || statuses.Value[0] == nil {
		return nil, rpc.ErrNotFound
	}
	return statuses.Value[0], nil
}

func signatureConfirmed(status *rpc.SignatureStatusesResult) bool {
	if status == nil || status.Err != nil {
		return false
	}

	switch status.ConfirmationStatus {
	case rpc.ConfirmationStatusConfirmed, rpc.ConfirmationStatusFinalized:
		return true
	default:
		return status.Confirmations == nil
	}
}

func formatAirdropError(cluster string, endpoint string, err error) error {
	if strings.EqualFold(cluster, "testnet") {
		return fmt.Errorf("request airdrop via %s: %w (testnet faucet/public RPC can be flaky; retry, override -cluster-rpc, or try -cluster devnet)", endpoint, err)
	}
	return fmt.Errorf("request airdrop via %s: %w", endpoint, err)
}
