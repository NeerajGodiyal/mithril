package replay

import "testing"

func TestApplyCapitalizationDelta(t *testing.T) {
	tests := []struct {
		name    string
		start   uint64
		delta   int64
		want    uint64
		wantErr bool
	}{
		{name: "positive", start: 10, delta: 5, want: 15},
		{name: "negative", start: 10, delta: -4, want: 6},
		{name: "zero", start: 10, delta: 0, want: 10},
		{name: "underflow", start: 3, delta: -4, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyCapitalizationDelta(tc.start, tc.delta)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("unexpected result: got %d want %d", got, tc.want)
			}
		})
	}
}
