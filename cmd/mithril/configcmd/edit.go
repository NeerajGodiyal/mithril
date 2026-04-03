package configcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var EditCmd = cobra.Command{
	Use:   "edit",
	Short: "Interactively edit configuration",
	Long:  "Opens an interactive editor for the current config file. Pre-fills with existing values.",
	Run: func(cmd *cobra.Command, args []string) {
		runConfigEdit()
	},
}

func init() {
	ConfigCmd.AddCommand(&EditCmd)
	EditCmd.Flags().StringVarP(&configFile, "config", "c", "config.toml", "Path to config file")
}

var (
	editTeal = lipgloss.Color("85")
	editDim  = lipgloss.Color("#666666")
	editText = lipgloss.Color("#EEEEEE")
)

func editTheme() *huh.Theme {
	t := huh.ThemeBase()
	t.Focused.Title = t.Focused.Title.Foreground(editTeal).Bold(true)
	t.Focused.Description = t.Focused.Description.Foreground(editDim)
	t.Focused.Base = t.Focused.Base.BorderForeground(editTeal)
	t.Focused.TextInput.Cursor = t.Focused.TextInput.Cursor.Foreground(editTeal)
	t.Focused.TextInput.Placeholder = t.Focused.TextInput.Placeholder.Foreground(editDim)
	t.Focused.SelectSelector = t.Focused.SelectSelector.Foreground(editTeal)
	t.Focused.SelectedOption = t.Focused.SelectedOption.Foreground(editTeal)
	t.Focused.FocusedButton = t.Focused.FocusedButton.Background(editTeal).Foreground(lipgloss.Color("#000000"))
	t.Focused.BlurredButton = t.Focused.BlurredButton.Foreground(editDim)
	t.Blurred.Title = t.Blurred.Title.Foreground(editDim)
	t.Blurred.TextInput.Text = t.Blurred.TextInput.Text.Foreground(editText)
	return t
}

func runConfigEdit() {
	if _, err := os.Stat(configFile); err != nil {
		fmt.Printf("Config file not found: %s\nRun: mithril setup\n", configFile)
		return
	}

	// Load existing config
	v := viper.New()
	v.SetConfigFile(configFile)
	if err := v.ReadInConfig(); err != nil {
		fmt.Printf("Error reading config: %v\n", err)
		return
	}

	// Read current values
	cluster := v.GetString("network.cluster")
	if cluster == "" {
		cluster = "mainnet-beta"
	}
	rpcSlice := v.GetStringSlice("network.rpc")
	rpcEndpoint := ""
	if len(rpcSlice) > 0 {
		rpcEndpoint = rpcSlice[0]
	}
	blockSource := v.GetString("block.source")
	if blockSource == "" {
		blockSource = "rpc"
	}
	lbEnabled := v.GetBool("lightbringer.enabled")
	gossipEntry := v.GetString("lightbringer.gossip_entrypoint")
	accountsPath := v.GetString("storage.accounts")
	snapshotsPath := v.GetString("storage.snapshots")
	logsPath := v.GetString("storage.logs")
	if logsPath == "" {
		logsPath = v.GetString("log.dir")
	}
	txpar := v.GetString("replay.txpar")
	if txpar == "" {
		txpar = fmt.Sprintf("%d", runtime.NumCPU()*2)
	}
	blockMaxRPS := v.GetString("block.max_rps")
	if blockMaxRPS == "" {
		blockMaxRPS = "8"
	}
	blockInflight := v.GetString("block.max_inflight")
	if blockInflight == "" {
		blockInflight = "8"
	}
	rpcPort := v.GetString("rpc.port")
	if rpcPort == "" {
		rpcPort = "8899"
	}
	logLevel := v.GetString("log.level")
	if logLevel == "" {
		logLevel = "info"
	}
	bootstrapMode := v.GetString("bootstrap.mode")
	if bootstrapMode == "" {
		bootstrapMode = "auto"
	}

	theme := editTheme()

	for {
	// ── Which section to edit? ──────────────────────────────────────
	var section string

	err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("◎ Edit Configuration").
				Description(fmt.Sprintf("Editing: %s\nPick a section to edit. Save & exit when done.", configFile)).
				Options(
					huh.NewOption(fmt.Sprintf("Network      cluster=%s  rpc=%s", cluster, truncate(rpcEndpoint, 40)), "network"),
					huh.NewOption(fmt.Sprintf("Lightbringer enabled=%v", lbEnabled), "lightbringer"),
					huh.NewOption(fmt.Sprintf("Storage      accounts=%s", truncate(accountsPath, 35)), "storage"),
					huh.NewOption(fmt.Sprintf("Replay       txpar=%s", txpar), "replay"),
					huh.NewOption(fmt.Sprintf("Block Fetch  rps=%s  inflight=%s", blockMaxRPS, blockInflight), "block"),
					huh.NewOption(fmt.Sprintf("RPC Server   port=%s", rpcPort), "rpc"),
					huh.NewOption(fmt.Sprintf("Logging      level=%s", logLevel), "log"),
					huh.NewOption(fmt.Sprintf("Bootstrap    mode=%s", bootstrapMode), "bootstrap"),
					huh.NewOption("Save & exit", "save"),
				).
				Value(&section),
		),
	).WithTheme(theme).Run()
	if err != nil {
		return
	}

	// ── Edit the selected section ───────────────────────────────────
	switch section {
	case "network":
		err = huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().Title("Cluster").Options(
				huh.NewOption("───────────────────── ← Back", ""),
				huh.NewOption("mainnet-beta", "mainnet-beta"),
				huh.NewOption("testnet", "testnet"),
				huh.NewOption("devnet", "devnet"),
			).Value(&cluster),
		)).WithTheme(theme).Run()
		if cluster == "" { cluster = v.GetString("network.cluster"); continue }
		err = huh.NewForm(huh.NewGroup(
			huh.NewInput().Title("RPC Endpoint").Value(&rpcEndpoint),
		)).WithTheme(theme).Run()

	case "lightbringer":
		var choice string
		if lbEnabled { choice = "enable" } else { choice = "disable" }
		err = huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().Title("Lightbringer").Options(
				huh.NewOption("───────────────────── ← Back", "back"),
				huh.NewOption("Enable — streams blocks via Turbine", "enable"),
				huh.NewOption("Disable — RPC only", "disable"),
			).Value(&choice),
		)).WithTheme(theme).Run()
		if choice == "back" { continue }
		lbEnabled = choice == "enable"
		if lbEnabled {
			err = huh.NewForm(huh.NewGroup(
				huh.NewInput().Title("Gossip Entrypoint").Placeholder("185.26.11.165:8000").Value(&gossipEntry),
			)).WithTheme(theme).Run()
		}

	case "storage":
		var choice string
		err = huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().Title("Storage Paths").Options(
				huh.NewOption("───────────────────── ← Back", "back"),
				huh.NewOption(fmt.Sprintf("Edit AccountsDB  (%s)", truncate(accountsPath, 30)), "accounts"),
				huh.NewOption(fmt.Sprintf("Edit Snapshots   (%s)", truncate(snapshotsPath, 30)), "snapshots"),
				huh.NewOption(fmt.Sprintf("Edit Logs        (%s)", truncate(logsPath, 30)), "logs"),
			).Value(&choice),
		)).WithTheme(theme).Run()
		if choice == "back" { continue }
		switch choice {
		case "accounts":
			err = huh.NewForm(huh.NewGroup(huh.NewInput().Title("AccountsDB Path").Value(&accountsPath))).WithTheme(theme).Run()
		case "snapshots":
			err = huh.NewForm(huh.NewGroup(huh.NewInput().Title("Snapshots Path").Value(&snapshotsPath))).WithTheme(theme).Run()
		case "logs":
			err = huh.NewForm(huh.NewGroup(huh.NewInput().Title("Logs Path").Value(&logsPath))).WithTheme(theme).Run()
		}

	case "replay":
		var choice string
		err = huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().Title("Replay").Options(
				huh.NewOption("───────────────────── ← Back", "back"),
				huh.NewOption(fmt.Sprintf("Edit parallelism (current: %s)", txpar), "edit"),
			).Value(&choice),
		)).WithTheme(theme).Run()
		if choice == "back" { continue }
		err = huh.NewForm(huh.NewGroup(
			huh.NewInput().Title("Transaction Parallelism").
				Description(fmt.Sprintf("Recommended: %d (2x CPU cores)", runtime.NumCPU()*2)).
				Value(&txpar).Validate(func(s string) error {
					if _, err := strconv.Atoi(strings.TrimSpace(s)); err != nil { return fmt.Errorf("must be a number") }
					return nil
				}),
		)).WithTheme(theme).Run()

	case "block":
		var choice string
		err = huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().Title("Block Fetch").Options(
				huh.NewOption("───────────────────── ← Back", "back"),
				huh.NewOption(fmt.Sprintf("Edit Max RPS (current: %s)", blockMaxRPS), "edit"),
			).Value(&choice),
		)).WithTheme(theme).Run()
		if choice == "back" { continue }
		err = huh.NewForm(huh.NewGroup(
			huh.NewInput().Title("Max RPS").Value(&blockMaxRPS),
			huh.NewInput().Title("Max Inflight").Value(&blockInflight),
		)).WithTheme(theme).Run()

	case "rpc":
		var choice string
		err = huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().Title("RPC Server").Options(
				huh.NewOption("───────────────────── ← Back", "back"),
				huh.NewOption(fmt.Sprintf("Edit port (current: %s)", rpcPort), "edit"),
			).Value(&choice),
		)).WithTheme(theme).Run()
		if choice == "back" { continue }
		err = huh.NewForm(huh.NewGroup(
			huh.NewInput().Title("Mithril RPC Port").Description("0 to disable").Value(&rpcPort),
		)).WithTheme(theme).Run()

	case "log":
		err = huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().Title("Log Level").Options(
				huh.NewOption("───────────────────── ← Back", ""),
				huh.NewOption("debug", "debug"),
				huh.NewOption("info (recommended)", "info"),
				huh.NewOption("warn", "warn"),
				huh.NewOption("error", "error"),
			).Value(&logLevel),
		)).WithTheme(theme).Run()
		if logLevel == "" { logLevel = v.GetString("log.level"); continue }

	case "bootstrap":
		err = huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().Title("Bootstrap Mode").Options(
				huh.NewOption("───────────────────── ← Back", ""),
				huh.NewOption("auto — use existing or download snapshot", "auto"),
				huh.NewOption("snapshot — rebuild from snapshot", "snapshot"),
				huh.NewOption("new-snapshot — always download fresh", "new-snapshot"),
				huh.NewOption("accountsdb — require existing data", "accountsdb"),
			).Value(&bootstrapMode),
		)).WithTheme(theme).Run()
		if bootstrapMode == "" { bootstrapMode = v.GetString("bootstrap.mode"); continue }

	case "save":
		// Break out of loop to save
	}

	if err != nil {
		return
	}

	if section == "save" {
		break
	}
	} // end loop

	// ── Write back ──────────────────────────────────────────────────
	// Re-read original file to preserve comments and structure
	data, err := os.ReadFile(configFile)
	if err != nil {
		fmt.Printf("Error reading config: %v\n", err)
		return
	}

	// Apply changes using line-by-line replacement
	content := string(data)
	content = setTomlValue(content, "network", "cluster", fmt.Sprintf("%q", cluster))
	content = setTomlValue(content, "network", "rpc", fmt.Sprintf("[%q]", rpcEndpoint))
	content = setTomlValue(content, "storage", "accounts", fmt.Sprintf("%q", filepath.Clean(accountsPath)))
	content = setTomlValue(content, "storage", "snapshots", fmt.Sprintf("%q", filepath.Clean(snapshotsPath)))
	content = setTomlValue(content, "block", "max_rps", blockMaxRPS)
	content = setTomlValue(content, "block", "max_inflight", blockInflight)
	content = setTomlValue(content, "replay", "txpar", txpar)
	content = setTomlValue(content, "rpc", "port", rpcPort)
	content = setTomlValue(content, "log", "level", fmt.Sprintf("%q", logLevel))
	content = setTomlValue(content, "bootstrap", "mode", fmt.Sprintf("%q", bootstrapMode))

	if lbEnabled {
		content = setTomlValue(content, "block", "source", "\"lightbringer\"")
		// Ensure [lightbringer] section exists
		if !strings.Contains(content, "[lightbringer]") {
			content += fmt.Sprintf("\n[lightbringer]\nenabled = true\nbinary_path = \"./lightbringer\"\ngossip_entrypoint = %q\ngrpc_addr = \"127.0.0.1:3001\"\nrpc_addr = \"127.0.0.1:3000\"\n", gossipEntry)
		} else {
			content = setTomlValue(content, "lightbringer", "enabled", "true")
			if gossipEntry != "" {
				content = setTomlValue(content, "lightbringer", "gossip_entrypoint", fmt.Sprintf("%q", gossipEntry))
			}
		}
	} else {
		content = setTomlValue(content, "block", "source", "\"rpc\"")
		if strings.Contains(content, "[lightbringer]") {
			content = setTomlValue(content, "lightbringer", "enabled", "false")
		}
	}

	if logsPath != "" {
		content = setTomlValue(content, "storage", "logs", fmt.Sprintf("%q", filepath.Clean(logsPath)))
		content = setTomlValue(content, "log", "dir", fmt.Sprintf("%q", filepath.Clean(logsPath)))
	}

	if err := os.WriteFile(configFile, []byte(content), 0600); err != nil {
		fmt.Printf("Error writing config: %v\n", err)
		return
	}

	successStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	fmt.Printf("\n  %s Config updated: %s\n\n", successStyle.Render("✓"), configFile)
}

// setTomlValue replaces a value in a TOML file, preserving structure
func setTomlValue(content, section, key, value string) string {
	lines := strings.Split(content, "\n")
	inSection := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Track sections
		if strings.HasPrefix(trimmed, "[") && !strings.HasPrefix(trimmed, "[[") {
			sectionName := strings.Trim(trimmed, "[] ")
			inSection = sectionName == section
			continue
		}

		if inSection && strings.HasPrefix(trimmed, key+" ") || inSection && strings.HasPrefix(trimmed, key+"=") {
			// Preserve indentation
			indent := ""
			for _, c := range line {
				if c == ' ' || c == '\t' {
					indent += string(c)
				} else {
					break
				}
			}
			lines[i] = fmt.Sprintf("%s%s = %s", indent, key, value)
			return strings.Join(lines, "\n")
		}
	}
	return content // key not found, return unchanged
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
