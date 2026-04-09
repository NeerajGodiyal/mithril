package statuscmd

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

var (
	accountsPath string

	StatusCmd = cobra.Command{
		Use:   "status",
		Short: "Show current Mithril node status",
		Long:  "Check if Mithril is running, what slot it's at, and Lightbringer connectivity.",
		Run: func(cmd *cobra.Command, args []string) {
			runStatus()
		},
	}
)

func init() {
	StatusCmd.Flags().StringVar(&accountsPath, "accounts", "", "Path to AccountsDB directory (to find state file)")
}

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("85"))
	successStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("85"))
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#6c6c6c"))
	valueStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("255"))
)

// mithrilState mirrors the relevant fields from pkg/state/state.go
type mithrilState struct {
	SnapshotSlot   uint64 `json:"snapshot_slot"`
	LastSlot       uint64 `json:"last_slot"`
	LastBankhash   string `json:"last_bankhash"`
	GenesisHash    string `json:"genesis_hash"`
	ShutdownReason string `json:"last_shutdown_reason"`
}

func runStatus() {
	fmt.Println()
	fmt.Println(titleStyle.Render("◎ Mithril Status"))
	fmt.Println()

	// Try to find state file
	stateFound := false
	searchPaths := []string{accountsPath}
	if accountsPath == "" {
		searchPaths = []string{
			"/mnt/mithril-accounts",
			"./data/accounts",
			".",
		}
	}

	var state mithrilState
	var statePath string
	for _, dir := range searchPaths {
		p := filepath.Join(dir, "mithril_state.json")
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(data, &state); err != nil {
			continue
		}
		statePath = p
		stateFound = true
		break
	}

	if stateFound {
		fmt.Printf("  %s State file: %s\n", successStyle.Render("✓"), dimStyle.Render(statePath))
		fmt.Printf("  %s Last slot:  %s\n", successStyle.Render("✓"), valueStyle.Render(fmt.Sprintf("%d", state.LastSlot)))
		if state.SnapshotSlot > 0 {
			fmt.Printf("  %s Snapshot:   %s\n", dimStyle.Render("-"), valueStyle.Render(fmt.Sprintf("slot %d", state.SnapshotSlot)))
		}
		if state.ShutdownReason != "" {
			fmt.Printf("  %s Last stop:  %s\n", dimStyle.Render("-"), valueStyle.Render(state.ShutdownReason))
		}
		if state.LastBankhash != "" {
			short := state.LastBankhash
			if len(short) > 12 {
				short = short[:12] + "..."
			}
			fmt.Printf("  %s Bankhash:   %s\n", dimStyle.Render("-"), dimStyle.Render(short))
		}
	} else {
		fmt.Printf("  %s No state file found\n", warnStyle.Render("~"))
		fmt.Printf("    %s Mithril hasn't run yet, or --accounts path is wrong\n", dimStyle.Render(""))
	}

	fmt.Println()

	// Check Mithril RPC
	fmt.Println("  " + dimStyle.Render("Services:"))
	rpcAddr := "127.0.0.1:8899"
	conn, err := net.DialTimeout("tcp", rpcAddr, 2*time.Second)
	if err == nil {
		conn.Close()
		fmt.Printf("  %s Mithril RPC responding on %s\n", successStyle.Render("✓"), rpcAddr)
	} else {
		fmt.Printf("  %s Mithril RPC not responding on %s\n", dimStyle.Render("-"), rpcAddr)
	}

	// Check Lightbringer gRPC
	lbAddr := "127.0.0.1:3001"
	conn, err = net.DialTimeout("tcp", lbAddr, 2*time.Second)
	if err == nil {
		conn.Close()
		fmt.Printf("  %s Lightbringer gRPC responding on %s\n", successStyle.Render("✓"), lbAddr)
	} else {
		fmt.Printf("  %s Lightbringer gRPC not responding on %s\n", dimStyle.Render("-"), lbAddr)
	}

	// Check Lightbringer HTTP
	lbHTTP := "127.0.0.1:3000"
	conn, err = net.DialTimeout("tcp", lbHTTP, 2*time.Second)
	if err == nil {
		conn.Close()
		fmt.Printf("  %s Lightbringer HTTP responding on %s\n", successStyle.Render("✓"), lbHTTP)
	} else {
		fmt.Printf("  %s Lightbringer HTTP not responding on %s\n", dimStyle.Render("-"), lbHTTP)
	}

	fmt.Println()
}
