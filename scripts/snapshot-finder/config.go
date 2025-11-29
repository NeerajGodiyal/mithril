package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
)

// Config holds runtime options (JSON-configurable)
type Config struct {
	ThreadsCount     int      `json:"threads_count"`
	RPCAddress       string   `json:"rpc_address"`
	MaxSnapshotAge   int      `json:"max_snapshot_age"`
	MinDownloadSpeed int      `json:"min_download_speed"` // MB/s threshold as int (we will compare in bytes/sec)
	MaxDownloadSpeed int      `json:"max_download_speed"`
	MaxLatency       int      `json:"max_latency"` // ms
	Version          string   `json:"version"`
	WithPrivateRPC   bool     `json:"with_private_rpc"`
	MeasurementTime  int      `json:"measurement_time"` // seconds
	SnapshotPath     string   `json:"snapshot_path"`
	Sleep            int      `json:"sleep"` // seconds
	SortOrder        string   `json:"sort_order"`
	Blacklist        []string `json:"blacklist"`
	Verbose          bool     `json:"verbose"`
}

var DefaultConfig = Config{
	ThreadsCount:     1000,
	RPCAddress:       "https://api.mainnet-beta.solana.com",
	MaxSnapshotAge:   1300,
	MinDownloadSpeed: 60,
	MaxDownloadSpeed: 0,
	MaxLatency:       40,
	Version:          "3.0.11",
	WithPrivateRPC:   false,
	MeasurementTime:  7,
	SnapshotPath:     ".",
	Sleep:            30,
	SortOrder:        "slots_diff",
	Blacklist:        []string{},
	Verbose:          false,
}

// LoadConfig reads JSON config from path and merges with defaults.
func LoadConfig() (Config, error) {
	cfg := DefaultConfig
	// by default will look for a file under config/config.json
	path := "config/config.json"
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("Config file %s not found or unreadable, using default config: %v", path, err)
		return cfg, nil
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("Failed to decode config file %s, using default config: %v", path, err)
		return cfg, fmt.Errorf("failed to decode config file %s: %w", path, err)
	}

	log.Printf("Loaded custom config")
	return cfg, nil
}
