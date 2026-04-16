package lightbringer

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const configFileName = "Lightbringer.toml"

// LightbringerTOML represents the Lightbringer.toml structure that Lightbringer expects.
// This mirrors the Rust ConfigRaw struct in the Lightbringer source.
type LightbringerTOML struct {
	GossipEntrypoint string
	Storage          string
	RpcAddr          string
	GrpcAddr         string

	// Optional sections
	InfluxdbHost     string
	InfluxdbDatabase string
	InfluxdbToken    string

	BlockConfirmRpcHTTP string
	BlockConfirmRpcWS   string
}

// Validate checks that required fields are present and well-formed.
func (c *LightbringerTOML) Validate() error {
	if c.GossipEntrypoint == "" {
		return fmt.Errorf("gossip_entrypoint is required")
	}
	if err := validateHostPort(c.GossipEntrypoint, "gossip_entrypoint"); err != nil {
		return err
	}
	if c.GrpcAddr == "" {
		return fmt.Errorf("grpc_addr is required")
	}
	if err := validateHostPort(c.GrpcAddr, "grpc_addr"); err != nil {
		return err
	}
	if c.RpcAddr != "" {
		if err := validateHostPort(c.RpcAddr, "rpc_addr"); err != nil {
			return err
		}
	}
	return nil
}

// validateHostPort checks that addr is a valid host:port with a numeric port in range.
func validateHostPort(addr, field string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s %q is not valid host:port: %w", field, addr, err)
	}
	if host == "" {
		return fmt.Errorf("%s has empty host", field)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("%s port %q is not numeric", field, portStr)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s port %d is out of range 1-65535", field, port)
	}
	return nil
}

// GenerateTOML produces a valid Lightbringer.toml string from the config.
func (c *LightbringerTOML) GenerateTOML() string {
	var b strings.Builder

	fmt.Fprintf(&b, "gossip_entrypoint = %q\n", c.GossipEntrypoint)
	fmt.Fprintf(&b, "storage = %q\n", c.Storage)
	fmt.Fprintf(&b, "rpc_addr = %q\n", c.RpcAddr)
	fmt.Fprintf(&b, "grpc_addr = %q\n", c.GrpcAddr)

	if c.InfluxdbHost != "" {
		b.WriteString("\n[influxdb]\n")
		fmt.Fprintf(&b, "host = %q\n", c.InfluxdbHost)
		fmt.Fprintf(&b, "database = %q\n", c.InfluxdbDatabase)
		fmt.Fprintf(&b, "token = %q\n", c.InfluxdbToken)
	}

	if c.BlockConfirmRpcHTTP != "" {
		b.WriteString("\n[block_confirmation]\n")
		fmt.Fprintf(&b, "rpc_http = %q\n", c.BlockConfirmRpcHTTP)
		fmt.Fprintf(&b, "rpc_websocket = %q\n", c.BlockConfirmRpcWS)
	}

	return b.String()
}

// WriteConfigFile writes Lightbringer.toml to the given directory using an atomic
// write pattern (write to temp file, then rename) to prevent partial writes.
func (c *LightbringerTOML) WriteConfigFile(dir string) (string, error) {
	content := c.GenerateTOML()
	targetPath := filepath.Join(dir, configFileName)

	// Write to temp file in same directory (required for atomic rename on same filesystem).
	// CreateTemp uses mode 0600; we keep this restrictive since the file may contain tokens.
	tmpFile, err := os.CreateTemp(dir, "Lightbringer.toml.tmp.*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp config file: %w", err)
	}
	// Ensure restrictive permissions regardless of umask
	if err := tmpFile.Chmod(0600); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("failed to set config file permissions: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.WriteString(content); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to write config content: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to sync config file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to close temp config file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, targetPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to rename config file: %w", err)
	}

	return targetPath, nil
}
