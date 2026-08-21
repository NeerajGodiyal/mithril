package mcp

import (
	"bytes"
	"context"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectServerForTest(t *testing.T, server *mcpsdk.Server, clientName string) *mcpsdk.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: clientName, Version: "0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		serverSession.Close()
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	})
	return clientSession
}

func connectNewServerForTest(t *testing.T, cfg Config, telemetry *bytes.Buffer) *mcpsdk.ClientSession {
	t.Helper()
	return connectServerForTest(t, newServerWithTelemetry(cfg, newTelemetryWriter(telemetry)), "test-client")
}

func toolResultText(result *mcpsdk.CallToolResult) string {
	var text strings.Builder
	for _, content := range result.Content {
		if item, ok := content.(*mcpsdk.TextContent); ok {
			text.WriteString(item.Text)
		}
	}
	return text.String()
}
