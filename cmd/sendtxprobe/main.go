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
	"github.com/gagliardetto/solana-go/rpc"
)

const defaultCluster = "mainnet-beta"

type config struct {
	mithrilRPC       string
	cluster          string
	clusterRPC       string
	senderPrivateKey string
	memoText         string
	skipPreflight    bool
	timeout          time.Duration
	pollInterval     time.Duration
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
	flag.StringVar(&cfg.cluster, "cluster", defaultCluster, "Cluster for confirmation: mainnet-beta, mainnet, testnet, or devnet")
	flag.StringVar(&cfg.clusterRPC, "cluster-rpc", "", "Override the public cluster RPC URL used for confirmation")
	flag.StringVar(&cfg.senderPrivateKey, "sender-private-key", "", "Base58 sender private key used to sign the probe transaction")
	flag.StringVar(&cfg.memoText, "memo", "", "Optional memo payload; defaults to a generated probe string")
	flag.BoolVar(&cfg.skipPreflight, "skip-preflight", false, "Pass skipPreflight=true to Mithril sendTransaction")
	flag.DurationVar(&cfg.timeout, "timeout", 3*time.Minute, "Max time to wait for transaction confirmation")
	flag.DurationVar(&cfg.pollInterval, "poll-interval", 2*time.Second, "Polling interval for signature confirmation")
	flag.Parse()

	if cfg.mithrilRPC == "" {
		return config{}, errors.New("must provide -mithril-rpc")
	}
	if cfg.senderPrivateKey == "" {
		return config{}, errors.New("must provide -sender-private-key")
	}
	if cfg.timeout <= 0 {
		return config{}, errors.New("-timeout must be greater than zero")
	}
	if cfg.pollInterval <= 0 {
		return config{}, errors.New("-poll-interval must be greater than zero")
	}

	cluster, err := canonicalClusterName(cfg.cluster)
	if err != nil {
		return config{}, err
	}
	cfg.cluster = cluster

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

	sender, err := loadSenderWallet(cfg.senderPrivateKey)
	if err != nil {
		return err
	}

	fmt.Printf("Mithril RPC: %s\n", cfg.mithrilRPC)
	fmt.Printf("Cluster: %s\n", cfg.cluster)
	fmt.Printf("Cluster RPC: %s\n", cfg.clusterRPC)
	fmt.Printf("Sender: %s\n", sender.PublicKey())

	preBalance, err := clusterClient.GetBalance(ctx, sender.PublicKey(), rpc.CommitmentConfirmed)
	if err != nil {
		return fmt.Errorf("get sender balance from %s: %w", cfg.clusterRPC, err)
	}
	fmt.Printf("Sender balance before submit: %d lamports\n", preBalance.Value)

	latestBlockhash, err := mithrilClient.GetLatestBlockhash(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		return fmt.Errorf("getLatestBlockhash via Mithril %s: %w", cfg.mithrilRPC, err)
	}
	if latestBlockhash == nil || latestBlockhash.Value == nil {
		return errors.New("Mithril getLatestBlockhash returned no value")
	}
	fmt.Printf("Mithril latest blockhash: %s\n", latestBlockhash.Value.Blockhash)

	memoText := cfg.memoText
	if memoText == "" {
		memoText = fmt.Sprintf("mithril sendtxprobe %s", time.Now().UTC().Format(time.RFC3339Nano))
	}
	fmt.Printf("Memo payload: %q\n", memoText)

	instruction := buildMemoInstruction(sender.PublicKey(), memoText)

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

	if err := waitForSignature(ctx, clusterClient, txSig, cfg.timeout, cfg.pollInterval, "memo"); err != nil {
		return err
	}

	postBalance, err := clusterClient.GetBalance(ctx, sender.PublicKey(), rpc.CommitmentConfirmed)
	if err != nil {
		return fmt.Errorf("get sender post-submit balance from %s: %w", cfg.clusterRPC, err)
	}
	fmt.Printf("Sender balance after submit: %d lamports\n", postBalance.Value)
	if postBalance.Value > preBalance.Value {
		fmt.Printf("Balance delta: +%d lamports\n", postBalance.Value-preBalance.Value)
	} else {
		fmt.Printf("Balance delta: -%d lamports\n", preBalance.Value-postBalance.Value)
	}

	fmt.Printf("sendTransaction flow succeeded through Mithril.\n")
	return nil
}

func canonicalClusterName(cluster string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(cluster)) {
	case "mainnet", "mainnet-beta":
		return "mainnet-beta", nil
	case "testnet":
		return "testnet", nil
	case "devnet":
		return "devnet", nil
	default:
		return "", fmt.Errorf("unsupported -cluster %q; expected mainnet-beta, mainnet, testnet, or devnet", cluster)
	}
}

func clusterRPCFor(cluster string) (string, error) {
	switch cluster {
	case "mainnet-beta":
		return rpc.MainNetBeta_RPC, nil
	case "testnet":
		return rpc.TestNet_RPC, nil
	case "devnet":
		return rpc.DevNet_RPC, nil
	default:
		return "", fmt.Errorf("unsupported -cluster %q; expected mainnet-beta, mainnet, testnet, or devnet", cluster)
	}
}

func loadSenderWallet(privateKey string) (*solana.Wallet, error) {
	wallet, err := solana.WalletFromPrivateKeyBase58(privateKey)
	if err != nil {
		return nil, fmt.Errorf("load sender private key: %w", err)
	}
	return wallet, nil
}

func buildMemoInstruction(sender solana.PublicKey, memoText string) solana.Instruction {
	return solana.NewInstruction(
		solana.MemoProgramID,
		solana.AccountMetaSlice{
			solana.Meta(sender).SIGNER(),
		},
		[]byte(memoText),
	)
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

