package blockstream

import "testing"

// qosThrottleSuspected must fire on the real serve-repair freeze signature
// (pushing hard, was answered well, answers collapsed) and stay quiet on the
// look-alikes: steady scarcity (never answered well) and a latency spike (which
// shifts timely -> late, so ANSWER rate — timely+late — stays up even as timely
// drops). See qosThrottleSuspected.
func TestQoSThrottleSuspected(t *testing.T) {
	cases := []struct {
		name                             string
		sendRate, prevAnswer, answerRate float64
		want                             bool
	}{
		{"freeze: was served, answers cratered", 5000, 800, 50, true},
		{"healthy: answers holding", 5000, 800, 780, false},
		{"not pushing: send below floor", 100, 800, 5, false},
		{"steady scarcity: never answered well", 5000, 40, 5, false},
		{"latency spike: timely fell but late kept answers up", 5000, 800, 700, false},
		{"partial dip above the collapse fraction", 5000, 800, 300, false},
		{"exactly at the collapse fraction is not a collapse", 5000, 1000, 350, false},
		{"just under the collapse fraction trips", 5000, 1000, 349, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := qosThrottleSuspected(tc.sendRate, tc.prevAnswer, tc.answerRate); got != tc.want {
				t.Fatalf("qosThrottleSuspected(%.0f, %.0f, %.0f) = %v, want %v",
					tc.sendRate, tc.prevAnswer, tc.answerRate, got, tc.want)
			}
		})
	}
}
