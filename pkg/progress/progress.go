package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

const (
	barWidth       = 40
	updateInterval = 500 * time.Millisecond
	ewmaTau        = 5.0 // seconds for EWMA smoothing

	// Teal/cyan color
	colorTeal  = "\x1b[38;2;0;188;212m"
	colorReset = "\x1b[0m"
	colorDim   = "\x1b[2m"

	// Cursor control
	clearLine = "\x1b[2K"
	moveUp    = "\x1b[1A"

	// Size constants
	gib = 1 << 30
)

// ProgressBar tracks progress for a single operation
type ProgressBar struct {
	label     string
	total     int64
	current   atomic.Int64
	startTime time.Time

	// For EWMA throughput calculation
	mu           sync.Mutex
	lastUpdate   time.Time
	lastBytes    int64
	ewmaThroughput float64
}

// NewProgressBar creates a new progress bar with the given label
func NewProgressBar(label string) *ProgressBar {
	return &ProgressBar{
		label:     label,
		startTime: time.Now(),
	}
}

// SetTotal sets the expected total bytes
func (p *ProgressBar) SetTotal(total int64) {
	atomic.StoreInt64(&p.total, total)
}

// Add increments the current progress by n bytes
func (p *ProgressBar) Add(n int64) {
	p.current.Add(n)
}

// Current returns the current progress
func (p *ProgressBar) Current() int64 {
	return p.current.Load()
}

// Total returns the total expected
func (p *ProgressBar) Total() int64 {
	return atomic.LoadInt64(&p.total)
}

// updateThroughput calculates EWMA throughput
func (p *ProgressBar) updateThroughput() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	current := p.current.Load()

	if p.lastUpdate.IsZero() {
		p.lastUpdate = now
		p.lastBytes = current
		return 0
	}

	elapsed := now.Sub(p.lastUpdate).Seconds()
	if elapsed < 0.1 {
		return p.ewmaThroughput
	}

	bytesPerSec := float64(current-p.lastBytes) / elapsed

	// EWMA: weight = 1 - e^(-elapsed/tau)
	weight := 1.0 - exp(-elapsed/ewmaTau)
	if p.ewmaThroughput == 0 {
		p.ewmaThroughput = bytesPerSec
	} else {
		p.ewmaThroughput = weight*bytesPerSec + (1-weight)*p.ewmaThroughput
	}

	p.lastUpdate = now
	p.lastBytes = current
	return p.ewmaThroughput
}

// Simple exp approximation to avoid importing math
func exp(x float64) float64 {
	if x > 0 {
		return 1 + x + x*x/2 + x*x*x/6
	}
	inv := 1 - x + x*x/2 - x*x*x/6
	if inv <= 0 {
		return 0
	}
	return 1 / inv
}

// Render returns the progress bar string
func (p *ProgressBar) Render(useColor bool) string {
	current := p.current.Load()
	total := atomic.LoadInt64(&p.total)

	var percent float64
	if total > 0 {
		percent = float64(current) / float64(total) * 100
		if percent > 100 {
			percent = 100
		}
	}

	throughput := p.updateThroughput()

	// Calculate ETA
	var eta string
	if throughput > 0 && total > current {
		remaining := float64(total-current) / throughput
		eta = formatDuration(time.Duration(remaining * float64(time.Second)))
	} else if current >= total && total > 0 {
		eta = "done"
	} else {
		eta = "--:--"
	}

	// Build the bar
	filled := int(percent / 100 * float64(barWidth))
	if filled > barWidth {
		filled = barWidth
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)

	// Format throughput
	throughputStr := formatThroughput(throughput)

	// Format size progress (current/total in GB)
	sizeStr := formatSizeProgress(current, total)

	// Build the line with size progress
	if useColor {
		return fmt.Sprintf("%s%-18s%s [%s%s%s] %5.1f%% %13s %8s  ETA %s",
			colorTeal, p.label, colorReset,
			colorTeal, bar, colorReset,
			percent, sizeStr, throughputStr, eta)
	}
	return fmt.Sprintf("%-18s [%s] %5.1f%% %13s %8s  ETA %s",
		p.label, bar, percent, sizeStr, throughputStr, eta)
}

func formatThroughput(bytesPerSec float64) string {
	if bytesPerSec < 1024 {
		return fmt.Sprintf("%.0f B/s", bytesPerSec)
	} else if bytesPerSec < 1024*1024 {
		return fmt.Sprintf("%.1f KB/s", bytesPerSec/1024)
	} else {
		return fmt.Sprintf("%.1f MB/s", bytesPerSec/(1024*1024))
	}
}

func formatSizeProgress(current, total int64) string {
	currentGB := float64(current) / float64(gib)
	totalGB := float64(total) / float64(gib)
	if total > 0 {
		return fmt.Sprintf("%5.1f/%5.1f GB", currentGB, totalGB)
	}
	return fmt.Sprintf("%5.1f/  ??? GB", currentGB)
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		return "--:--"
	}
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm%02ds", m, s)
}

// DualProgress manages two progress bars displayed simultaneously
type DualProgress struct {
	Download *ProgressBar
	Build    *ProgressBar

	mu       sync.Mutex
	started  bool
	done     bool
	stopCh   chan struct{}
	doneCh   chan struct{}
	output   io.Writer
	useColor bool
}

// NewDualProgress creates a new dual progress display
func NewDualProgress() *DualProgress {
	// Check if stdout is a TTY
	useColor := term.IsTerminal(int(os.Stdout.Fd()))

	return &DualProgress{
		Download: NewProgressBar("Snapshot Download"),
		Build:    NewProgressBar("AccountsDB Build"),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
		output:   os.Stdout,
		useColor: useColor,
	}
}

// Start begins the progress display update loop
func (d *DualProgress) Start() {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return
	}
	d.started = true
	d.mu.Unlock()

	// Print initial empty lines
	fmt.Fprintln(d.output)
	fmt.Fprintln(d.output)

	go d.updateLoop()
}

// updateLoop periodically updates the display
func (d *DualProgress) updateLoop() {
	ticker := time.NewTicker(updateInterval)
	defer ticker.Stop()
	defer close(d.doneCh)

	for {
		select {
		case <-d.stopCh:
			d.render() // Final render
			return
		case <-ticker.C:
			d.render()
		}
	}
}

// render updates the display
func (d *DualProgress) render() {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Move up two lines and clear
	if d.useColor {
		fmt.Fprint(d.output, moveUp+clearLine)
		fmt.Fprint(d.output, moveUp+clearLine)
	}

	// Render both bars
	fmt.Fprintln(d.output, d.Download.Render(d.useColor))
	fmt.Fprintln(d.output, d.Build.Render(d.useColor))
}

// Stop stops the progress display
func (d *DualProgress) Stop() {
	d.mu.Lock()
	if d.done {
		d.mu.Unlock()
		return
	}
	d.done = true
	d.mu.Unlock()

	close(d.stopCh)
	<-d.doneCh
}

// PrintBanner prints the Mithril ASCII art banner
func PrintBanner() {
	banner := `
               _______ __________________          _______ _________ _
              (       )\__   __/\__   __/|\     /|(  ____ )\__   __/( \
    .         | () () |   ) (      ) (   | )   ( || (    )|   ) (   | (            .
  ./|\.       | || || |   | |      | |   | (___) || (____)|   | |   | |          ./|\.
 <--:-->      | |(_)| |   | |      | |   |  ___  ||     __)   | |   | |         <--:-->
  '\|/'       | |   | |   | |      | |   | (   ) || (\ (      | |   | |          '\|/'
    '         | )   ( |___) (___   | |   | )   ( || ) \ \_____) (___| (____/\      '
              |/     \|\_______/   )_(   |/     \||/   \__/\_______/(_______/
`

	useColor := term.IsTerminal(int(os.Stdout.Fd()))

	lines := strings.Split(banner, "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		if useColor {
			fmt.Printf("%s%s%s\n", colorTeal, line, colorReset)
		} else {
			fmt.Println(line)
		}
	}
	fmt.Println()
}

// PrintSnapshotSourceSummary prints a clean summary box for the selected snapshot source
func PrintSnapshotSourceSummary(nodeIP string, slot int, referenceSlot int, nodeVersion string, speedMBs float64, searchDuration time.Duration) {
	useColor := term.IsTerminal(int(os.Stdout.Fd()))

	// Calculate age in slots
	age := referenceSlot - slot

	// Format version (truncate if too long)
	version := nodeVersion
	if version == "" {
		version = "unknown"
	}

	fmt.Println()
	if useColor {
		fmt.Printf("%s", colorTeal)
	}
	fmt.Println("  ┌────────────────────────────────────────────────────────────┐")
	fmt.Printf("  │  %-58s│\n", "✓ Full Snapshot Source Selected")
	fmt.Println("  │                                                            │")
	fmt.Printf("  │    %-12s %-44s│\n", "IP:", nodeIP)
	fmt.Printf("  │    %-12s %-44d│\n", "Slot:", slot)
	fmt.Printf("  │    %-12s %-44s│\n", "Age:", fmt.Sprintf("%d slots behind", age))
	fmt.Printf("  │    %-12s %-44s│\n", "Version:", version)
	fmt.Printf("  │    %-12s %-44s│\n", "Speed:", fmt.Sprintf("%.1f MB/s", speedMBs))
	fmt.Printf("  │    %-12s %-44s│\n", "Found in:", searchDuration.Round(time.Second).String())
	fmt.Println("  └────────────────────────────────────────────────────────────┘")
	if useColor {
		fmt.Printf("%s", colorReset)
	}
	fmt.Println()
}
