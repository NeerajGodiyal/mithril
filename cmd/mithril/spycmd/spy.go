package spycmd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/config"
	"github.com/Overclock-Validator/mithril/pkg/gossip"
	"github.com/gagliardetto/solana-go"
	"github.com/spf13/cobra"
)

var (
	spyDuration         time.Duration
	spyMinNodes         int
	spyGossipEntrypoint string
	spyGossipBind       string
	spyTurbineBind      string
	spyAlpenglowBind    string
	spyAdvertisedIP     string
	spyShredVersion     uint16
	spyIdentityKeypair  string
	spyJSON             bool
)

var SpyCmd = cobra.Command{
	Use:   "spy",
	Short: "Join gossip and inspect discovered ContactInfo records",
	Long: `Join the cluster gossip mesh using the same gossip client as mithril run,
collect verified ContactInfo records from peers, and print a summary table once
enough nodes are discovered (or when duration expires).

This is a passive diagnostic command; it does not replay banks or produce blocks.`,
	RunE: runSpy,
}

func init() {
	SpyCmd.Flags().DurationVar(&spyDuration, "duration", 60*time.Second, "How long to listen (0 = until interrupted)")
	SpyCmd.Flags().IntVar(&spyMinNodes, "min-nodes", 50, "Wait for at least this many discovered contacts before printing")
	SpyCmd.Flags().StringVar(&spyGossipEntrypoint, "gossip-entrypoint", "", "Solana gossip entrypoint (default: config turbine.gossip_entrypoint)")
	SpyCmd.Flags().StringVar(&spyGossipBind, "gossip-bind-addr", "", "UDP address for gossip traffic (default: config turbine.gossip_bind_addr)")
	SpyCmd.Flags().StringVar(&spyTurbineBind, "turbine-bind-addr", "", "TVU bind address advertised in gossip (default: config turbine.bind_addr or block.turbine_bind_addr)")
	SpyCmd.Flags().StringVar(&spyAlpenglowBind, "alpenglow-bind-addr", "", "Alpenglow observer bind address for gossip ContactInfo (default: config consensus.alpenglow_observer_bind_addr)")
	SpyCmd.Flags().StringVar(&spyAdvertisedIP, "advertised-ip", "", "Public IP advertised in gossip (default: config turbine.advertised_ip)")
	SpyCmd.Flags().Uint16Var(&spyShredVersion, "shred-version", 0, "Shred version override (0 = config or entrypoint discovery)")
	SpyCmd.Flags().StringVar(&spyIdentityKeypair, "identity-keypair", "", "Validator identity keypair for gossip (default: config validator.identity_keypair; ephemeral if unset)")
	SpyCmd.Flags().BoolVar(&spyJSON, "json", false, "Print discovered contacts as JSON")
}

func runSpy(cmd *cobra.Command, _ []string) error {
	if err := config.InitConfig(); err != nil {
		return err
	}

	opts := spyOptionsFromConfig(cmd)
	if opts.gossipEntrypoint == "" {
		return fmt.Errorf("spy requires --gossip-entrypoint or turbine.gossip_entrypoint")
	}
	if opts.minNodes <= 0 {
		return fmt.Errorf("--min-nodes must be > 0")
	}

	identity, identityPubkey, err := loadSpyIdentity(opts.identityKeypair)
	if err != nil {
		return err
	}

	client, err := gossip.NewClient(gossip.Config{
		Entrypoint:    opts.gossipEntrypoint,
		BindAddr:      opts.gossipBind,
		TVUAddr:       opts.turbineBind,
		AlpenglowAddr: opts.alpenglowBind,
		AdvertisedIP:  opts.advertisedIP,
		ShredVersion:  opts.shredVersion,
		Identity:      identity,
		Name:          gossip.ClientName,
	})
	if err != nil {
		return fmt.Errorf("gossip client: %w", err)
	}

	ctx := cmd.Context()
	if opts.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.duration)
		defer cancel()
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Run(ctx)
	}()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	fmt.Printf("gossip spy listening: entrypoint=%s bind=%s tvu=%s identity=%s min_nodes=%d duration=%s\n",
		opts.gossipEntrypoint, opts.gossipBind, opts.turbineBind, identityPubkey, opts.minNodes, formatDuration(opts.duration))

waitLoop:
	for {
		select {
		case <-ctx.Done():
			break waitLoop
		case err := <-errCh:
			if err != nil && ctx.Err() == nil {
				return fmt.Errorf("gossip client stopped: %w", err)
			}
			break waitLoop
		case <-ticker.C:
			stats := client.Stats()
			summary := client.SummarizeDiscoveredContacts()
			fmt.Printf("spy progress: discovered=%d tvu_peers=%d accepted=%d rx_contacts=%d gossip_peers=%d repair_peers=%d\n",
				summary.Total, summary.TVUPeers, stats.AcceptedContacts, stats.RxContacts, stats.Peers, stats.RepairPeers)
			if summary.Total >= opts.minNodes {
				break waitLoop
			}
		}
	}

	return printSpyResults(client, opts)
}

func printSpyResults(client *gossip.Client, opts spyOptions) error {
	stats := client.Stats()
	summary := client.SummarizeDiscoveredContacts()
	contacts := client.DiscoveredContacts()

	if spyJSON {
		payload := struct {
			Summary  gossip.DiscoveredContactSummary `json:"summary"`
			Stats    gossip.Stats                    `json:"stats"`
			Contacts []jsonContact                   `json:"contacts"`
		}{
			Summary:  summary,
			Stats:    stats,
			Contacts: make([]jsonContact, 0, len(contacts)),
		}
		for _, contact := range contacts {
			sockets := make(map[string]string, len(contact.Sockets))
			for tag, addr := range contact.Sockets {
				sockets[gossip.SocketTagName(tag)] = addr
			}
			payload.Contacts = append(payload.Contacts, jsonContact{
				Pubkey:      contact.PubkeyString(),
				ShredVer:    contact.ShredVer,
				Wallclock:   contact.Wallclock,
				Gossip:      contact.Gossip,
				ServeRepair: contact.ServeRepair,
				TVU:         contact.TVU,
				Sockets:     sockets,
				LastSeen:    contact.LastSeen.UTC().Format(time.RFC3339),
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	fmt.Printf("\ngossip spy summary: discovered=%d with_gossip=%d with_repair=%d with_tvu=%d tvu_peers=%d\n",
		summary.Total, summary.WithGossip, summary.WithRepair, summary.WithTVU, summary.TVUPeers)
	fmt.Printf("gossip stats: rx=%d accepted=%d rx_contacts=%d pull_requests=%d pull_responses=%d decode_errors=%d\n",
		stats.RxPackets, stats.AcceptedContacts, stats.RxContacts, stats.RxPullRequests, stats.TxPullResponses, stats.RxDecodeErrors)
	if len(summary.UniqueTags) > 0 {
		fmt.Printf("socket tags seen: %s\n", formatTagCounts(summary.UniqueTags))
	}

	fmt.Printf("\n%-44s %5s  %-22s  %-22s  %-22s  sockets\n",
		"pubkey", "shred", "gossip", "repair", "tvu")
	for _, contact := range contacts {
		fmt.Printf("%-44s %5d  %-22s  %-22s  %-22s  %s\n",
			contact.PubkeyString(),
			contact.ShredVer,
			contact.Gossip,
			contact.ServeRepair,
			contact.TVU,
			formatSocketSummary(contact.Sockets),
		)
	}
	return nil
}

type jsonContact struct {
	Pubkey      string            `json:"pubkey"`
	ShredVer    uint16            `json:"shred_version"`
	Wallclock   uint64            `json:"wallclock"`
	Gossip      string            `json:"gossip"`
	ServeRepair string            `json:"serve_repair"`
	TVU         string            `json:"tvu"`
	Sockets     map[string]string `json:"sockets"`
	LastSeen    string            `json:"last_seen"`
}

type spyOptions struct {
	duration         time.Duration
	minNodes         int
	gossipEntrypoint string
	gossipBind       string
	turbineBind      string
	alpenglowBind    string
	advertisedIP     string
	shredVersion     uint16
	identityKeypair  string
}

func spyOptionsFromConfig(cmd *cobra.Command) spyOptions {
	turbineBind := stringFlag(cmd, "turbine-bind-addr", "block.turbine_bind_addr")
	if turbineBind == "" {
		turbineBind = stringFlag(cmd, "turbine-bind-addr", "turbine.bind_addr")
	}
	if turbineBind == "" {
		turbineBind = "0.0.0.0:8001"
	}

	gossipBind := stringFlag(cmd, "gossip-bind-addr", "turbine.gossip_bind_addr")
	if gossipBind == "" {
		gossipBind = "0.0.0.0:8000"
	}

	alpenglowBind := stringFlag(cmd, "alpenglow-bind-addr", "consensus.alpenglow_observer_bind_addr")

	return spyOptions{
		duration:         spyDuration,
		minNodes:         spyMinNodes,
		gossipEntrypoint: stringFlag(cmd, "gossip-entrypoint", "turbine.gossip_entrypoint"),
		gossipBind:       gossipBind,
		turbineBind:      turbineBind,
		alpenglowBind:    alpenglowBind,
		advertisedIP:     stringFlag(cmd, "advertised-ip", "turbine.advertised_ip"),
		shredVersion:     uint16Flag(cmd, "shred-version", "turbine.shred_version"),
		identityKeypair:  stringFlag(cmd, "identity-keypair", "validator.identity_keypair"),
	}
}

func loadSpyIdentity(path string) (ed25519.PrivateKey, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		_, identity, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, "", fmt.Errorf("generate ephemeral gossip identity: %w", err)
		}
		pubkey := solana.PublicKeyFromBytes(identity.Public().(ed25519.PublicKey))
		return identity, pubkey.String() + " (ephemeral)", nil
	}
	key, err := solana.PrivateKeyFromSolanaKeygenFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("load identity keypair %s: %w", path, err)
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, "", fmt.Errorf("identity keypair %s has invalid private key size %d", path, len(key))
	}
	identity := ed25519.PrivateKey(append([]byte(nil), key...))
	return identity, key.PublicKey().String(), nil
}

func stringFlag(cmd *cobra.Command, flagName, configKey string) string {
	if cmd.Flags().Changed(flagName) {
		value, _ := cmd.Flags().GetString(flagName)
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(config.GetString(configKey))
}

func uint16Flag(cmd *cobra.Command, flagName, configKey string) uint16 {
	if cmd.Flags().Changed(flagName) {
		value, _ := cmd.Flags().GetUint16(flagName)
		return value
	}
	value := config.GetInt(configKey)
	if value < 0 || value > 0xffff {
		return 0
	}
	return uint16(value)
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "until interrupted"
	}
	return d.String()
}

func formatTagCounts(counts map[string]int) string {
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sortStrings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", name, counts[name]))
	}
	return strings.Join(parts, " ")
}

func formatSocketSummary(sockets map[uint8]string) string {
	if len(sockets) == 0 {
		return "none"
	}
	tags := make([]uint8, 0, len(sockets))
	for tag := range sockets {
		tags = append(tags, tag)
	}
	sortUint8s(tags)
	parts := make([]string, 0, len(tags))
	for _, tag := range tags {
		parts = append(parts, fmt.Sprintf("%s=%s", gossip.SocketTagName(tag), sockets[tag]))
	}
	return strings.Join(parts, " ")
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j-1] > values[j]; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}

func sortUint8s(values []uint8) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j-1] > values[j]; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}
