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

var baseStyle = lipgloss.NewStyle().
	BorderStyle(lipgloss.NormalBorder()).
	BorderForeground(lipgloss.Color("240"))

type model struct {
	table      table.Model
	url        string
	err        error
	lastUpdate time.Time
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
		}
	case metricsMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.table.SetRows(msg.rows)
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
	return baseStyle.Render(m.table.View()) + fmt.Sprintf("\nLast updated: %s\nPress q to quit.", m.lastUpdate.Format(time.TimeOnly))
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
