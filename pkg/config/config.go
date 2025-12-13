package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// LedgerConfig holds ledger-related configuration (matches Firedancer [ledger] section)
type LedgerConfig struct {
	Path                string `toml:"path" mapstructure:"path"`                                 // was: blockdir
	AccountsPath        string `toml:"accounts_path" mapstructure:"accounts_path"`               // was: out
	SnapshotArchivePath string `toml:"snapshot_archive_path" mapstructure:"snapshot_archive_path"` // was: path
	IncrementalSnapshot string `toml:"incremental_snapshot" mapstructure:"incremental_snapshot"` // was: incremental-snapshot-filename
}

// RpcConfig holds RPC-related configuration (matches Firedancer [rpc] section)
type RpcConfig struct {
	Rpc  []string `toml:"rpc" mapstructure:"rpc"` // List of RPC endpoints
	Port int      `toml:"port" mapstructure:"port"` // was: rpc-server-port
}

// ReplayConfig holds replay-related configuration
type ReplayConfig struct {
	Txpar              int64 `toml:"txpar" mapstructure:"txpar"`                             // UNCHANGED
	NumSlots           int64 `toml:"num_slots" mapstructure:"num_slots"`                     // was: num-replay-slots
	EndSlot            int64 `toml:"end_slot" mapstructure:"end_slot"`                       // was: endslot
	LoadFromSnapshot   bool  `toml:"load_from_snapshot" mapstructure:"load_from_snapshot"`   // was: snapshot
	LoadFromAccountsDb bool  `toml:"load_from_accounts_db" mapstructure:"load_from_accounts_db"` // was: accountsdb
}

// PprofConfig holds pprof-related configuration
type PprofConfig struct {
	Port           int64  `toml:"port" mapstructure:"port"`                       // was: pprofport
	CpuProfilePath string `toml:"cpu_profile_path" mapstructure:"cpu_profile_path"` // was: cpuprof-filename
}

// DebugConfig holds debug-related configuration
type DebugConfig struct {
	TransactionSignatures []string `toml:"transaction_signatures" mapstructure:"transaction_signatures"` // was: debugtx
	AccountWrites         []string `toml:"account_writes" mapstructure:"account_writes"`                 // was: debugacctwrites
}

// DevelopmentConfig holds development/tuning configuration (matches Firedancer [development] section)
type DevelopmentConfig struct {
	ZstdDecoderConcurrency   int         `toml:"zstd_decoder_concurrency" mapstructure:"zstd_decoder_concurrency"`     // was: zstd-decoder-concurrency
	MaxConcurrentFlushers    int         `toml:"max_concurrent_flushers" mapstructure:"max_concurrent_flushers"`       // was: max-concurrent-flushers
	ParamArenaSizeMB         uint64      `toml:"param_arena_size_mb" mapstructure:"param_arena_size_mb"`               // was: param-arena-size-mb
	BorrowedAccountArenaSize uint64      `toml:"borrowed_account_arena_size" mapstructure:"borrowed_account_arena_size"` // was: borrowed-account-arena-size
	UsePool                  bool        `toml:"use_pool" mapstructure:"use_pool"`                                     // was: use-pool
	Pprof                    PprofConfig `toml:"pprof" mapstructure:"pprof"`
	Debug                    DebugConfig `toml:"debug" mapstructure:"debug"`
}

// ReportingConfig holds metrics/reporting configuration (matches Firedancer [reporting] section)
type ReportingConfig struct {
	MetricsPath string `toml:"metrics_path" mapstructure:"metrics_path"` // was: metrics-filename
}

// OvercastConfig holds Overcast-related configuration
type OvercastConfig struct {
	Endpoint             string `toml:"endpoint" mapstructure:"endpoint"`                           // was: overcast
	DownloadSnapshotPath string `toml:"download_snapshot_path" mapstructure:"download_snapshot_path"` // was: download-snapshot
}

// Config holds all configuration options for Mithril (Firedancer-style hierarchy)
type Config struct {
	// Top-level (matches Firedancer style)
	Name             string `toml:"name" mapstructure:"name"`
	ScratchDirectory string `toml:"scratch_directory" mapstructure:"scratch_directory"` // was: scratchdir

	// Sections
	Ledger      LedgerConfig      `toml:"ledger" mapstructure:"ledger"`
	Rpc         RpcConfig         `toml:"rpc" mapstructure:"rpc"`
	Replay      ReplayConfig      `toml:"replay" mapstructure:"replay"`
	Development DevelopmentConfig `toml:"development" mapstructure:"development"`
	Reporting   ReportingConfig   `toml:"reporting" mapstructure:"reporting"`
	Overcast    OvercastConfig    `toml:"overcast" mapstructure:"overcast"`
}

// ConfigFile holds the path to the config file (set via --config flag)
var ConfigFile string

// InitConfig loads configuration from TOML file.
// If no --config flag is provided, defaults to "config.toml" in current directory.
// CLI flag precedence is handled separately in initConfigAndBindFlags.
func InitConfig() error {
	configPath := ConfigFile
	if configPath == "" {
		configPath = "config.toml" // Default config file
	}

	// Get the directory and filename
	dir := filepath.Dir(configPath)
	filename := filepath.Base(configPath)

	// Remove extension for viper
	ext := filepath.Ext(filename)
	name := strings.TrimSuffix(filename, ext)

	viper.SetConfigName(name)
	viper.SetConfigType("toml")
	viper.AddConfigPath(dir)

	// Try to read config file (not an error if default doesn't exist)
	if err := viper.ReadInConfig(); err != nil {
		// Only return error if user explicitly specified a config file
		if ConfigFile != "" {
			return fmt.Errorf("error reading config file: %w", err)
		}
		// Default config.toml not found is fine, just continue without it
	}

	// Note: We don't bind flags here - precedence is handled manually
	// by checking cmd.Flags().Lookup(name).Changed in initConfigAndBindFlags

	return nil
}

// GetString returns a string value from viper (config file or flag)
func GetString(key string) string {
	return viper.GetString(key)
}

// GetInt returns an int value from viper (config file or flag)
func GetInt(key string) int {
	return viper.GetInt(key)
}

// GetInt64 returns an int64 value from viper (config file or flag)
func GetInt64(key string) int64 {
	return viper.GetInt64(key)
}

// GetUint64 returns a uint64 value from viper (config file or flag)
func GetUint64(key string) uint64 {
	return viper.GetUint64(key)
}

// GetBool returns a bool value from viper (config file or flag)
func GetBool(key string) bool {
	return viper.GetBool(key)
}

// GetStringSlice returns a string slice value from viper (config file or flag)
func GetStringSlice(key string) []string {
	return viper.GetStringSlice(key)
}

// IsSet returns true if a key has been set (either via config file or flag)
func IsSet(key string) bool {
	return viper.IsSet(key)
}
