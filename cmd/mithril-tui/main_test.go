package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
)

func TestFetchMetrics(t *testing.T) {
	// 1. Setup a mock Prometheus server
	mockResponse := `
# HELP go_goroutines Number of goroutines that currently exist.
# TYPE go_goroutines gauge
go_goroutines 15
# HELP http_requests_total Total number of HTTP requests made.
# TYPE http_requests_total counter
http_requests_total{method="post",code="200"} 1024
http_requests_total{method="get",code="404"} 12
`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, mockResponse)
	}))
	defer server.Close()

	// 2. Call the function
	cmd := fetchMetrics(server.URL)
	msg := cmd()

	// 3. Assertions
	metricsMsg, ok := msg.(metricsMsg)
	assert.True(t, ok, "Expected message to be of type metricsMsg")
	assert.NoError(t, metricsMsg.err)
	assert.Len(t, metricsMsg.rows, 3, "Expected 3 rows of metrics")

	// Verify specific rows (note: the function sorts them)
	// Row 0: go_goroutines
	assert.Equal(t, "go_goroutines", metricsMsg.rows[0][0])
	assert.Equal(t, "15", metricsMsg.rows[0][1])
	assert.Equal(t, "", metricsMsg.rows[0][2])

	// Row 1: http_requests_total (get, 404) - sorted by label
	assert.Equal(t, "http_requests_total", metricsMsg.rows[1][0])
	assert.Equal(t, "12", metricsMsg.rows[1][1])
	assert.Contains(t, metricsMsg.rows[1][2], `method="get"`)

	// Row 2: http_requests_total (post, 200)
	assert.Equal(t, "http_requests_total", metricsMsg.rows[2][0])
	assert.Equal(t, "1024", metricsMsg.rows[2][1])
	assert.Contains(t, metricsMsg.rows[2][2], `method="post"`)
}

func TestFetchMetrics_Error(t *testing.T) {
	// Test with an invalid URL
	cmd := fetchMetrics("http://invalid-url-that-does-not-exist")
	msg := cmd()

	metricsMsg, ok := msg.(metricsMsg)
	assert.True(t, ok)
	assert.Error(t, metricsMsg.err)
	assert.Nil(t, metricsMsg.rows)
}

func TestFetchMetrics_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cmd := fetchMetrics(server.URL)
	msg := cmd()

	metricsMsg, ok := msg.(metricsMsg)
	assert.True(t, ok)
	assert.Error(t, metricsMsg.err)
	assert.Contains(t, metricsMsg.err.Error(), "500 Internal Server Error")
}

func TestFilterRows(t *testing.T) {
	// Setup sample data
	rows := []table.Row{
		{"go_goroutines", "10", ""},
		{"process_cpu_seconds", "123.45", ""},
		{"promhttp_metric_handler_requests_total", "5", ""},
		{"mithril_block_height", "1000", ""},
		{"mithril_transaction_count", "500", ""},
		{"other_metric", "1", ""},
	}

	m := model{
		allRows: rows,
	}

	// Test FilterAll
	m.filterMode = FilterAll
	filtered := m.filterRows()
	assert.Len(t, filtered, 6)
	assert.Equal(t, rows, filtered)

	// Test FilterMachine
	m.filterMode = FilterMachine
	filtered = m.filterRows()
	assert.Len(t, filtered, 3)
	assert.Equal(t, "go_goroutines", filtered[0][0])
	assert.Equal(t, "process_cpu_seconds", filtered[1][0])
	assert.Equal(t, "promhttp_metric_handler_requests_total", filtered[2][0])

	// Test FilterMithril
	m.filterMode = FilterMithril
	filtered = m.filterRows()
	assert.Len(t, filtered, 3)
	assert.Equal(t, "mithril_block_height", filtered[0][0])
	assert.Equal(t, "mithril_transaction_count", filtered[1][0])
	assert.Equal(t, "other_metric", filtered[2][0])
}

// Mock tea.Msg for testing purposes if needed, though we cast directly above.
var _ tea.Msg = metricsMsg{}
