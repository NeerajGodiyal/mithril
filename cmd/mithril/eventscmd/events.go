// Package eventscmd provides the read-only rooted event CLI.
package eventscmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/config"
	"github.com/Overclock-Validator/mithril/pkg/rootedevents"
	"github.com/Overclock-Validator/mithril/pkg/rootedfeed"
	"github.com/gagliardetto/solana-go"
	"github.com/spf13/cobra"
)

// EventsCmd exports manifest-selected rooted events as JSON Lines.
var EventsCmd = newEventsCommand()

func newEventsCommand() *cobra.Command {
	var accountsPath, afterText, ownerText, accountText, mentionText string
	var latest, follow, framed bool
	cmd := &cobra.Command{
		Use:           "events",
		Short:         "Read rooted transactions and accounts for custom indexing",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		PreRunE: func(*cobra.Command, []string) error {
			if accountsPath != "" && config.ConfigFile == "" {
				return nil
			}
			return config.InitConfig()
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if latest && afterText != "" {
				return errors.New("use either --latest or --after, not both")
			}
			if mentionText != "" && (ownerText != "" || accountText != "") {
				return errors.New("--mention cannot be combined with --owner or --account")
			}
			if accountsPath == "" {
				accountsPath = config.GetString("storage.accounts")
			}
			if strings.TrimSpace(accountsPath) == "" {
				return errors.New("storage.accounts is empty; set it in config.toml or pass --accounts")
			}

			retain := uint64(65)
			if horizon := config.GetUint64("storage.rewind_horizon_batches"); horizon > 0 && horizon < ^uint64(0) {
				retain = horizon + 1
			}
			follower, err := rootedfeed.NewFollower(accountsPath, retain)
			if err != nil {
				return err
			}
			var after *rootedevents.Cursor
			if afterText != "" {
				cursor, err := parseCursor(afterText)
				if err != nil {
					return err
				}
				after = &cursor
			}
			if latest {
				cursor, err := follower.LatestCursor()
				if err != nil && !(follow && errors.Is(err, rootedfeed.ErrNoBatches)) {
					return err
				}
				after = cursor
			}

			owner, err := optionalPubkey(ownerText, "owner")
			if err != nil {
				return err
			}
			account, err := optionalPubkey(accountText, "account")
			if err != nil {
				return err
			}
			mention, err := optionalPubkey(mentionText, "mention")
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			emit := func(event rootedevents.Event) error {
				if !matches(event, owner, account, mention) {
					return nil
				}
				return encoder.Encode(&event)
			}
			emitMetadata := func(record rootedfeed.MetadataRecord) error {
				return encoder.Encode(&record)
			}

			for {
				var last *rootedevents.Cursor
				if framed {
					last, _, err = follower.StreamFramedAfter(after, emitMetadata, emit)
				} else {
					last, _, err = follower.StreamAfter(after, emit)
				}
				if err != nil && !(follow && errors.Is(err, rootedfeed.ErrNoBatches)) {
					return err
				}
				if last != nil {
					after = last
				}
				if !follow {
					return nil
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(time.Second):
				}
			}
		},
	}
	cmd.Flags().StringVar(&accountsPath, "accounts", "", "AccountsDB storage root, parent of accounts/ (overrides config)")
	cmd.Flags().StringVar(&afterText, "after", "", "Resume after cursor SLOT:ORDINAL")
	cmd.Flags().BoolVar(&latest, "latest", false, "Start after the newest retained event")
	cmd.Flags().BoolVar(&follow, "follow", false, "Wait for newly rooted events")
	cmd.Flags().BoolVar(&framed, "framed", false, "Include source, start, and manifest batch records")
	cmd.Flags().StringVar(&ownerText, "owner", "", "Only account updates whose post-state owner is this program")
	cmd.Flags().StringVar(&accountText, "account", "", "Only updates for this account")
	cmd.Flags().StringVar(&mentionText, "mention", "", "Only transactions mentioning this address")
	return cmd
}

func parseCursor(value string) (rootedevents.Cursor, error) {
	slotText, ordinalText, ok := strings.Cut(value, ":")
	if !ok || slotText == "" || ordinalText == "" {
		return rootedevents.Cursor{}, fmt.Errorf("cursor %q must use SLOT:ORDINAL", value)
	}
	slot, err := strconv.ParseUint(slotText, 10, 64)
	if err != nil {
		return rootedevents.Cursor{}, fmt.Errorf("cursor slot %q is invalid", slotText)
	}
	ordinal, err := strconv.ParseUint(ordinalText, 10, 32)
	if err != nil {
		return rootedevents.Cursor{}, fmt.Errorf("cursor ordinal %q is invalid", ordinalText)
	}
	return rootedevents.Cursor{Slot: slot, Ordinal: uint32(ordinal)}, nil
}

func optionalPubkey(value, name string) (*solana.PublicKey, error) {
	if value == "" {
		return nil, nil
	}
	key, err := solana.PublicKeyFromBase58(value)
	if err != nil {
		return nil, fmt.Errorf("%s public key is invalid: %w", name, err)
	}
	return &key, nil
}

func matches(event rootedevents.Event, owner, account, mention *solana.PublicKey) bool {
	if event.Kind == rootedevents.SlotRooted {
		return true
	}
	if event.Transaction != nil {
		if owner != nil || account != nil {
			return false
		}
		if mention == nil {
			return true
		}
		for _, key := range event.Transaction.AccountKeys {
			if key == mention.String() {
				return true
			}
		}
		return false
	}
	if event.Account == nil || mention != nil {
		return false
	}
	if owner != nil && event.Account.Owner != owner.String() {
		return false
	}
	return account == nil || event.Account.Pubkey == account.String()
}
