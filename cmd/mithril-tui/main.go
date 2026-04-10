package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	ViewMetricsAll = iota
	ViewMetricsMachine
	ViewMetricsMithril
	ViewMithrilLogs
	ViewLightbringerLogs
	viewCount // sentinel for modular cycling
)

const maxLogLines = 5000

var baseStyle = lipgloss.NewStyle().
	BorderStyle(lipgloss.NormalBorder()).
	BorderForeground(lipgloss.Color("240"))

type model struct {
	// Metrics view
	table      table.Model
	url        string
	err        error
	lastUpdate time.Time
	allRows    []table.Row

	// Log views
	mithrilLogPath      string
	lightbringerLogPath string
	mithrilLogLines     []string
	lightbringerLogLines []string
	logViewport         viewport.Model

	// State
	viewMode int
	width    int
	height   int
}

type tickMsg time.Time

type metricsMsg struct {
	rows []table.Row
	err  error
}

type logMsg struct {
	source string // "mithril" or "lightbringer"
	lines  []string
	err    error
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		fetchMetrics(m.url),
		tickCmd(),
	}
	if m.mithrilLogPath != "" {
		cmds = append(cmds, tailLog("mithril", m.mithrilLogPath))
	}
	if m.lightbringerLogPath != "" {
		cmds = append(cmds, tailLog("lightbringer", m.lightbringerLogPath))
	}
	return tea.Batch(cmds...)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.logViewport.Width = msg.Width - 2
		m.logViewport.Height = msg.Height - 4
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab":
			m.viewMode = (m.viewMode + 1) % viewCount
			// Skip unavailable log views (single loop with bound to prevent infinite cycling)
			for i := 0; i < viewCount; i++ {
				if (m.viewMode == ViewMithrilLogs && m.mithrilLogPath == "") ||
					(m.viewMode == ViewLightbringerLogs && m.lightbringerLogPath == "") {
					m.viewMode = (m.viewMode + 1) % viewCount
				} else {
					break
				}
			}
			if m.viewMode <= ViewMetricsMithril {
				m.table.SetRows(m.filterRows())
			} else {
				m.updateLogViewport()
			}
		}
	case metricsMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.allRows = msg.rows
		if m.viewMode <= ViewMetricsMithril {
			m.table.SetRows(m.filterRows())
		}
		m.lastUpdate = time.Now()
	case logMsg:
		if msg.err == nil {
			switch msg.source {
			case "mithril":
				m.mithrilLogLines = msg.lines
			case "lightbringer":
				m.lightbringerLogLines = msg.lines
			}
			if (msg.source == "mithril" && m.viewMode == ViewMithrilLogs) ||
				(msg.source == "lightbringer" && m.viewMode == ViewLightbringerLogs) {
				m.updateLogViewport()
			}
		}
	case tickMsg:
		cmds := []tea.Cmd{
			fetchMetrics(m.url),
			tickCmd(),
		}
		if m.mithrilLogPath != "" {
			cmds = append(cmds, tailLog("mithril", m.mithrilLogPath))
		}
		if m.lightbringerLogPath != "" {
			cmds = append(cmds, tailLog("lightbringer", m.lightbringerLogPath))
		}
		return m, tea.Batch(cmds...)
	}

	// Route input to the right component
	if m.viewMode <= ViewMetricsMithril {
		m.table, cmd = m.table.Update(msg)
	} else {
		m.logViewport, cmd = m.logViewport.Update(msg)
	}
	return m, cmd
}

func (m *model) updateLogViewport() {
	var lines []string
	switch m.viewMode {
	case ViewMithrilLogs:
		lines = m.mithrilLogLines
	case ViewLightbringerLogs:
		lines = m.lightbringerLogLines
	}
	content := strings.Join(lines, "\n")
	m.logViewport.SetContent(content)
	m.logViewport.GotoBottom()
}

func (m model) View() string {
	viewName := ""
	switch m.viewMode {
	case ViewMetricsAll:
		viewName = "All Metrics"
	case ViewMetricsMachine:
		viewName = "Machine Metrics (Go/Process)"
	case ViewMetricsMithril:
		viewName = "Mithril Custom Metrics"
	case ViewMithrilLogs:
		viewName = "Mithril Logs"
	case ViewLightbringerLogs:
		viewName = "Lightbringer Logs"
	}

	header := lipgloss.NewStyle().
		Foreground(lipgloss.Color("205")).
		Bold(true).
		Render(viewName)

	if m.viewMode <= ViewMetricsMithril {
		if m.err != nil {
			return fmt.Sprintf("Error fetching metrics: %v\n\nPress q to quit.", m.err)
		}
		return fmt.Sprintf("%s (Tab to switch)\n%s\nLast updated: %s | q to quit",
			header,
			baseStyle.Render(m.table.View()),
			m.lastUpdate.Format(time.TimeOnly))
	}

	// Log view
	return fmt.Sprintf("%s (Tab to switch) | scroll: up/down/pgup/pgdn\n%s\nq to quit",
		header,
		baseStyle.Render(m.logViewport.View()))
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

			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}

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

		sort.Slice(rows, func(i, j int) bool {
			if rows[i][0] == rows[j][0] {
				return rows[i][2] < rows[j][2]
			}
			return rows[i][0] < rows[j][0]
		})

		return metricsMsg{rows: rows}
	}
}

// tailLog reads the last maxLogLines from a log file.
// Only reads the last 64KB to avoid OOM on large log files.
func tailLog(source, path string) tea.Cmd {
	return func() tea.Msg {
		f, err := os.Open(path)
		if err != nil {
			return logMsg{source: source, err: err}
		}
		defer f.Close()

		stat, err := f.Stat()
		if err != nil {
			return logMsg{source: source, err: err}
		}

		// Read only the tail of the file to avoid loading huge logs into memory
		const tailSize = 64 * 1024
		offset := stat.Size() - tailSize
		if offset < 0 {
			offset = 0
		}

		buf := make([]byte, stat.Size()-offset)
		_, err = f.ReadAt(buf, offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return logMsg{source: source, err: err}
		}

		lines := strings.Split(string(buf), "\n")
		if len(lines) > maxLogLines {
			lines = lines[len(lines)-maxLogLines:]
		}

		return logMsg{source: source, lines: lines}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second*2, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m model) filterRows() []table.Row {
	if m.viewMode == ViewMetricsAll {
		return m.allRows
	}

	var filtered []table.Row
	for _, row := range m.allRows {
		name := row[0]
		isMachine := strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") || strings.HasPrefix(name, "promhttp_")

		if m.viewMode == ViewMetricsMachine && isMachine {
			filtered = append(filtered, row)
		} else if m.viewMode == ViewMetricsMithril && !isMachine {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func main() {
	url := flag.String("url", "http://localhost:9090/metrics", "Prometheus metrics URL")
	logDir := flag.String("log-dir", "", "Mithril log directory (uses 'latest' symlink to find current run)")
	flag.Parse()

	// Resolve log paths from the log directory
	mithrilLogPath := ""
	lightbringerLogPath := ""
	if *logDir != "" {
		latestDir := filepath.Join(*logDir, "latest")
		if target, err := os.Readlink(latestDir); err == nil {
			// Validate symlink target — reject traversal and absolute paths
			if !strings.Contains(target, "..") && !filepath.IsAbs(target) {
				runDir := filepath.Clean(filepath.Join(*logDir, target))
				cleanLogDir := filepath.Clean(*logDir) + string(os.PathSeparator)
				if strings.HasPrefix(runDir+string(os.PathSeparator), cleanLogDir) {
					ml := filepath.Join(runDir, "mithril.log")
					if _, err := os.Stat(ml); err == nil {
						mithrilLogPath = ml
					}
					ll := filepath.Join(runDir, "lightbringer.log")
					if _, err := os.Stat(ll); err == nil {
						lightbringerLogPath = ll
					}
				}
			}
		}
	}

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

	vp := viewport.New(80, 20)

	m := model{
		table:               t,
		url:                 *url,
		mithrilLogPath:      mithrilLogPath,
		lightbringerLogPath: lightbringerLogPath,
		logViewport:         vp,
	}

	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Println("Error running program:", err)
		os.Exit(1)
	}
}
