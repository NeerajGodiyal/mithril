package main

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

const dirPerm = 0755

func TestLoadConfigDefault(t *testing.T) {
	// Ensure config/config.json does not exist

	os.RemoveAll("config")
	os.MkdirAll("config", dirPerm)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if !reflect.DeepEqual(cfg, DefaultConfig) {
		t.Errorf("expected default config %+v, got %+v", DefaultConfig, cfg)
	}
}

func TestLoadConfigCustom(t *testing.T) {
	os.MkdirAll("config", dirPerm)
	defer os.RemoveAll("config")

	customConfig := Config{
		ThreadsCount:     10,
		RPCAddress:       "http://localhost:8899",
		MaxSnapshotAge:   500,
		MinDownloadSpeed: 20,
		MaxDownloadSpeed: 200,
		MaxLatency:       10,
		Version:          "3.0.12",
		WithPrivateRPC:   true,
		MeasurementTime:  5,
		SnapshotPath:     "/tmp/snapshots",
		Sleep:            15,
		SortOrder:        "random",
		Blacklist:        []string{"127.0.0.1"},
		Verbose:          true,
	}

	data, _ := json.Marshal(customConfig)
	if err := os.WriteFile("config/config.json", data, 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if !reflect.DeepEqual(cfg, customConfig) {
		t.Fatalf("expected config %+v, got %+v", customConfig, cfg)
	}
}

func TestLoadConfigInvalidJSON(t *testing.T) {
	os.MkdirAll("config", dirPerm)
	defer os.RemoveAll("config")

	// write invalid JSON
	if err := os.WriteFile("config/config.json", []byte("{invalid json}"), dirPerm); err != nil {
		t.Fatalf("failed to write invalid config file: %v", err)
	}

	_, err := LoadConfig()

	if err == nil {
		t.Fatal("expected an error for invalid JSON")
	}
}
