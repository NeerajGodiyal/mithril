# Mithril TUI

Mithril TUI is a lightweight terminal-based user interface for monitoring real-time metrics from a Mithril node. It connects to a Prometheus-compatible metrics endpoint and displays the data in an auto-refreshing table.

## Features

- **Real-time Monitoring**: Fetches metrics from your node's HTTP endpoint.
- **Auto-Refresh**: Updates the display every 2 seconds.
- **Clean Visualization**: Displays Metric Name, Value, and Labels in a structured table.
- **Sorting**: Automatically sorts metrics by name and labels for consistent viewing.

## Prerequisites

- Go 1.21 or higher
- A running Mithril node (or any application exposing Prometheus metrics)

## Installation & Build

To build the application from source:

```bash
# Navigate to the project root
cd mithril

# Build the binary
go build -o mithril-tui ./cmd/mithril-tui
```

## Usage

### Basic Usage

Ensure your Mithril node is running and exposing metrics (default is usually port 9090).

Run the TUI:

```bash
./mithril-tui
```

By default, it attempts to connect to `http://localhost:9090/metrics`.

### Custom Metrics URL

If your node is running on a different host or port, specify the URL using the `-url` flag:

```bash
./mithril-tui -url http://192.168.1.50:8080/metrics
```

### Controls

- **`q`** or **`Ctrl+C`**: Quit the application.
- **`Tab`**: Cycle through metric filters (All, Machine, Mithril).
- **`Up/Down Arrows`**: Scroll through the metrics list.

## Troubleshooting

- **"Error fetching metrics"**: Ensure the target URL is reachable and returning valid Prometheus text format metrics. Check if your node is running and the firewall allows connections to the metrics port.
