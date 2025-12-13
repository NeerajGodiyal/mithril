package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Config holds all configuration options for Mithril
type Config struct {
	// Paths
	Path                        string `toml:"path" mapstructure:"path"`
	OutputDir                   string `toml:"out" mapstructure:"out"`
	BlockDir                    string `toml:"blockdir" mapstructure:"blockdir"`
	ScratchDir                  string `toml:"scratchdir" mapstructure:"scratchdir"`

	// RPC
	RpcEndpoint      string `toml:"rpc" mapstructure:"rpc"`
	RpcEndpointFile  string `toml:"rpc_node_list" mapstructure:"rpc_node_list"`
	RpcServerPort    int    `toml:"rpc_server_port" mapstructure:"rpc_server_port"`
	OvercastEndpoint string `toml:"overcast" mapstructure:"overcast"`

	// Execution
	TxParallelism            int64  `toml:"txpar" mapstructure:"txpar"`
	ParamArenaSizeMB         uint64 `toml:"param_arena_size_mb" mapstructure:"param_arena_size_mb"`
	BorrowedAccountArenaSize uint64 `toml:"borrowed_account_arena_size" mapstructure:"borrowed_account_arena_size"`

	// Snapshot
	LoadFromSnapshot            bool   `toml:"snapshot" mapstructure:"snapshot"`
	LoadFromAccountsDb          bool   `toml:"accountsdb" mapstructure:"accountsdb"`
	IncrementalSnapshotFilename string `toml:"incremental_snapshot_filename" mapstructure:"incremental_snapshot_filename"`
	ZstdDecoderConcurrency      int    `toml:"zstd_decoder_concurrency" mapstructure:"zstd_decoder_concurrency"`
	MaxConcurrentFlushers       int    `toml:"max_concurrent_flushers" mapstructure:"max_concurrent_flushers"`
	DownloadSnapshot            string `toml:"download_snapshot" mapstructure:"download_snapshot"`

	// Replay
	NumReplaySlots int64 `toml:"num_replay_slots" mapstructure:"num_replay_slots"`
	EndSlot        int64 `toml:"endslot" mapstructure:"endslot"`

	// Debug/Profiling
	DebugTxs        []string `toml:"debugtx" mapstructure:"debugtx"`
	DebugAcctWrites []string `toml:"debugacctwrites" mapstructure:"debugacctwrites"`
	MetricsFilename string   `toml:"metrics_filename" mapstructure:"metrics_filename"`
	CpuprofFilename string   `toml:"cpuprof_filename" mapstructure:"cpuprof_filename"`
	PprofPort       int64    `toml:"pprofport" mapstructure:"pprofport"`

	// Misc
	UsePool bool `toml:"use_pool" mapstructure:"use_pool"`
}

// ConfigFile holds the path to the config file (set via --config flag)
var ConfigFile string

// InitConfig loads configuration from TOML file if specified, and binds to command flags.
// CLI flags take precedence over config file values.
func InitConfig(cmd *cobra.Command) error {
	if ConfigFile != "" {
		// Get the directory and filename
		dir := filepath.Dir(ConfigFile)
		filename := filepath.Base(ConfigFile)

		// Remove extension for viper
		ext := filepath.Ext(filename)
		name := strings.TrimSuffix(filename, ext)

		viper.SetConfigName(name)
		viper.SetConfigType("toml")
		viper.AddConfigPath(dir)

		if err := viper.ReadInConfig(); err != nil {
			return fmt.Errorf("error reading config file: %w", err)
		}
	}

	// Bind all flags to viper - CLI flags override config file values
	if err := viper.BindPFlags(cmd.Flags()); err != nil {
		return fmt.Errorf("error binding flags: %w", err)
	}

	return nil
}

// BindPersistentFlags binds persistent flags from parent commands
func BindPersistentFlags(cmd *cobra.Command) error {
	// Walk up the command tree and bind persistent flags
	for c := cmd; c != nil; c = c.Parent() {
		if err := viper.BindPFlags(c.PersistentFlags()); err != nil {
			return fmt.Errorf("error binding persistent flags: %w", err)
		}
	}
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


