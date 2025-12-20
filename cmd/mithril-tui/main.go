package main

import (
	"bufio"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	FilterAll = iota
	FilterMachine
	FilterMithril
)

var baseStyle = lipgloss.NewStyle().
	BorderStyle(lipgloss.NormalBorder()).
	BorderForeground(lipgloss.Color("240"))

type model struct {
	table      table.Model
	url        string
	err        error
	lastUpdate time.Time
	allRows    []table.Row
	filterMode int
}

type tickMsg time.Time

type metricsMsg struct {
	rows []table.Row
	err  error
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		fetchMetrics(m.url),
		tickCmd(),
	)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab":
			m.filterMode = (m.filterMode + 1) % 3
			m.table.SetRows(m.filterRows())
		}
	case metricsMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.allRows = msg.rows
		m.table.SetRows(m.filterRows())
		m.lastUpdate = time.Now()
	case tickMsg:
		return m, tea.Batch(
			fetchMetrics(m.url),
			tickCmd(),
		)
	}
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("Error fetching metrics: %v\n\nPress q to quit.", m.err)
	}

	filterName := "All Metrics"
	switch m.filterMode {
	case FilterMachine:
		filterName = "Machine Metrics (Go/Process)"
	case FilterMithril:
		filterName = "Mithril Custom Metrics"
	}

	header := lipgloss.NewStyle().
		Foreground(lipgloss.Color("205")).
		Bold(true).
		Render(filterName)

	return fmt.Sprintf("%s (Tab to switch metrics)\n%s\nLast updated: %s | q to quit",
		header,
		baseStyle.Render(m.table.View()),
		m.lastUpdate.Format(time.TimeOnly))
}

func fetchMetrics(url string) tea.Cmd {
	return func() tea.Msg {
		resp, err := http.Get(url)
		if err != nil {
			return metricsMsg{err: err}
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return metricsMsg{
				err: fmt.Errorf("metrics endpoint returned %s", resp.Status),
			}
		}

		var rows []table.Row

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())

			// Skip comments and empty lines
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}

			// Expected format:
			// metric_name{label="value"} 123.45
			// metric_name 123.45
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}

			sample := fields[0]
			value := fields[1]

			name := sample
			labels := ""

			if i := strings.Index(sample, "{"); i != -1 {
				name = sample[:i]
				labels = strings.TrimSuffix(sample[i+1:], "}")
			}

			rows = append(rows, table.Row{
				name,
				value,
				labels,
			})
		}

		if err := scanner.Err(); err != nil {
			return metricsMsg{err: err}
		}

		// Stable ordering for TUI
		sort.Slice(rows, func(i, j int) bool {
			if rows[i][0] == rows[j][0] {
				return rows[i][2] < rows[j][2]
			}
			return rows[i][0] < rows[j][0]
		})

		return metricsMsg{rows: rows}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second*2, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m model) filterRows() []table.Row {
	if m.filterMode == FilterAll {
		return m.allRows
	}

	var filtered []table.Row
	for _, row := range m.allRows {
		name := row[0]
		isMachine := strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") || strings.HasPrefix(name, "promhttp_")

		if m.filterMode == FilterMachine && isMachine {
			filtered = append(filtered, row)
		} else if m.filterMode == FilterMithril && !isMachine {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func main() {
	url := flag.String("url", "http://localhost:9090/metrics", "Prometheus metrics URL")
	flag.Parse()

	columns := []table.Column{
		{Title: "Metric", Width: 40},
		{Title: "Value", Width: 20},
		{Title: "Labels", Width: 40},
	}

	t := table.New(
		table.WithColumns(columns),
		table.WithFocused(true),
		table.WithHeight(20),
	)

	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("240")).
		BorderBottom(true).
		Bold(false)
	s.Selected = s.Selected.
		Foreground(lipgloss.Color("229")).
		Background(lipgloss.Color("57")).
		Bold(false)
	t.SetStyles(s)

	m := model{
		table: t,
		url:   *url,
	}

	if _, err := tea.NewProgram(m).Run(); err != nil {
		fmt.Println("Error running program:", err)
		os.Exit(1)
	}
}
