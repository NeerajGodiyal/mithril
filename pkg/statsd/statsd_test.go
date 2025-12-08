package statsd

import (
    package statsd

    import (
        "math"
        "testing"

        "github.com/prometheus/client_golang/prometheus/testutil"
    )

    // TestCountAndGauge ensures Count and Gauge update Prometheus metrics correctly.
    func TestCountAndGauge(t *testing.T) {
        // Count metric: SnapshotTarBytesRead is registered as CountT with no labels
        const wantCount = 5.0
        if err := Count(SnapshotTarBytesRead, int64(wantCount), nil, 1); err != nil {
            t.Fatalf("Count returned error: %v", err)
        }

        c := metricsCollection.counters[SnapshotTarBytesRead]
        if c == nil {
            t.Fatalf("counter for %v not registered", SnapshotTarBytesRead)
        }

        got := testutil.ToFloat64(c.WithLabelValues())
        if got != wantCount {
            t.Fatalf("counter value = %v, want %v", got, wantCount)
        }

        // Gauge metric: SnapshotWorkerPoolUtilization has one label (task)
        wantGauge := 0.73
        if err := Gauge(SnapshotWorkerPoolUtilization, wantGauge, []string{"index"}, 1); err != nil {
            t.Fatalf("Gauge returned error: %v", err)
        }

        g := metricsCollection.gauges[SnapshotWorkerPoolUtilization]
        if g == nil {
            t.Fatalf("gauge for %v not registered", SnapshotWorkerPoolUtilization)
        }

        gotg := testutil.ToFloat64(g.WithLabelValues("index"))
        if math.Abs(gotg-wantGauge) > 1e-9 {
            t.Fatalf("gauge value = %v, want %v", gotg, wantGauge)
        }
    }

    // TestDistributionAndTimingRegistration ensures histograms are registered and accept observations.
    func TestDistributionAndTimingRegistration(t *testing.T) {
        // Distribution metric: PreprocessBlock is registered with label "phase"
        if metricsCollection.histograms[PreprocessBlock] == nil {
            t.Fatalf("histogram for %v not registered", PreprocessBlock)
        }

        if err := Distribution(PreprocessBlock, 1.5, []string{"preprocess_block"}, 1); err != nil {
            t.Fatalf("Distribution returned error: %v", err)
        }

        // Timing: ensure it observes without panic
        if metricsCollection.histograms[TxLoop] == nil {
            t.Fatalf("histogram for %v not registered", TxLoop)
        }

        if err := Timing(TxLoop, 123456789, []string{"tx_loop"}, 1); err != nil {
            t.Fatalf("Timing returned error: %v", err)
        }
    }
    import (
        "math"
        "testing"

        "github.com/prometheus/client_golang/prometheus/testutil"
    )

    // TestCountAndGauge ensures Count and Gauge update Prometheus metrics correctly.
    func TestCountAndGauge(t *testing.T) {
        // Count metric: SnapshotTarBytesRead is registered as CountT with no labels
        const wantCount = 5.0
        if err := Count(SnapshotTarBytesRead, int64(wantCount), nil, 1); err != nil {
            t.Fatalf("Count returned error: %v", err)
        }

        c := metricsCollection.counters[SnapshotTarBytesRead]
        if c == nil {
            t.Fatalf("counter for %v not registered", SnapshotTarBytesRead)
        }

        got := testutil.ToFloat64(c.WithLabelValues())
        if got != wantCount {
            t.Fatalf("counter value = %v, want %v", got, wantCount)
        }

        // Gauge metric: SnapshotWorkerPoolUtilization has one label (task)
        wantGauge := 0.73
        if err := Gauge(SnapshotWorkerPoolUtilization, wantGauge, []string{"index"}, 1); err != nil {
            t.Fatalf("Gauge returned error: %v", err)
        }

        g := metricsCollection.gauges[SnapshotWorkerPoolUtilization]
        if g == nil {
            t.Fatalf("gauge for %v not registered", SnapshotWorkerPoolUtilization)
        }

        gotg := testutil.ToFloat64(g.WithLabelValues("index"))
        if math.Abs(gotg-wantGauge) > 1e-9 {
            t.Fatalf("gauge value = %v, want %v", gotg, wantGauge)
        }
    }

    // TestDistributionAndTimingRegistration ensures histograms are registered and accept observations.
    func TestDistributionAndTimingRegistration(t *testing.T) {
        // Distribution metric: PreprocessBlock is registered with label "phase"
        if metricsCollection.histograms[PreprocessBlock] == nil {
            t.Fatalf("histogram for %v not registered", PreprocessBlock)
        }

        if err := Distribution(PreprocessBlock, 1.5, []string{"preprocess_block"}, 1); err != nil {
            t.Fatalf("Distribution returned error: %v", err)
        }

        // Timing: ensure it observes without panic
        if metricsCollection.histograms[TxLoop] == nil {
            t.Fatalf("histogram for %v not registered", TxLoop)
        }

        if err := Timing(TxLoop, 123456789, []string{"tx_loop"}, 1); err != nil {
            t.Fatalf("Timing returned error: %v", err)
        }
    }
    "github.com/prometheus/client_golang/prometheus/testutil"
)

// TestCountAndGauge ensures Count and Gauge update Prometheus metrics correctly.
func TestCountAndGauge(t *testing.T) {
    // Count metric: SnapshotTarBytesRead is registered as CountT with no labels
    const wantCount = 5.0
    if err := Count(SnapshotTarBytesRead, int64(wantCount), nil, 1); err != nil {
        t.Fatalf("Count returned error: %v", err)
    }

    c := metricsCollection.counters[SnapshotTarBytesRead]
    if c == nil {
        t.Fatalf("counter for %v not registered", SnapshotTarBytesRead)
    }

    got := testutil.ToFloat64(c.WithLabelValues())
    if got != wantCount {
        t.Fatalf("counter value = %v, want %v", got, wantCount)
    }

    // Gauge metric: SnapshotWorkerPoolUtilization has one label (task)
    wantGauge := 0.73
    if err := Gauge(SnapshotWorkerPoolUtilization, wantGauge, []string{"index"}, 1); err != nil {
        t.Fatalf("Gauge returned error: %v", err)
    }

    g := metricsCollection.gauges[SnapshotWorkerPoolUtilization]
    if g == nil {
        t.Fatalf("gauge for %v not registered", SnapshotWorkerPoolUtilization)
    }

    gotg := testutil.ToFloat64(g.WithLabelValues("index"))
    if math.Abs(gotg-wantGauge) > 1e-9 {
        t.Fatalf("gauge value = %v, want %v", gotg, wantGauge)
    }
}

// TestDistributionAndTimingRegistration ensures histograms are registered and accept observations.
func TestDistributionAndTimingRegistration(t *testing.T) {
    // Distribution metric: PreprocessBlock is registered with label "phase"
    if metricsCollection.histograms[PreprocessBlock] == nil {
        t.Fatalf("histogram for %v not registered", PreprocessBlock)
    }

    if err := Distribution(PreprocessBlock, 1.5, []string{"preprocess_block"}, 1); err != nil {
        t.Fatalf("Distribution returned error: %v", err)
    }

    // Timing: ensure it observes without panic
    if metricsCollection.histograms[TxLoop] == nil {
        t.Fatalf("histogram for %v not registered", TxLoop)
    }

    if err := Timing(TxLoop, 123456789, []string{"tx_loop"}, 1); err != nil {
        t.Fatalf("Timing returned error: %v", err)
    }
}
