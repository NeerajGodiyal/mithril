package replay

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/state"
)

const (
	maxReplayDivergenceEvidence = 64
	maxDivergenceSignatureRunes = 96
	maxDivergenceDetailRunes    = 192
	maxDivergenceRecordedRunes  = 40
)

// ReplayDivergence identifies a confirmed execution mismatch. Its fields are
// bounded before they are logged or persisted.
type ReplayDivergence struct {
	Slot        uint64
	TxIndex     int
	TxSignature string
	Kind        string
	Detail      string
}

func (d *ReplayDivergence) Error() string {
	safe := boundedReplayDivergence(d)
	if safe == nil {
		return "replay divergence"
	}
	return fmt.Sprintf("replay divergence at slot %d tx %d (%s): %s — %s",
		safe.Slot, safe.TxIndex, safe.TxSignature, safe.Kind, safe.Detail)
}

func boundedReplayDivergence(d *ReplayDivergence) *ReplayDivergence {
	if d == nil {
		return nil
	}
	out := *d
	if out.TxIndex < -1 {
		out.TxIndex = -1
	}
	out.TxSignature = boundedDivergenceText(out.TxSignature, maxDivergenceSignatureRunes)
	out.Kind = replayDivergenceKind(out.Kind)
	out.Detail = boundedDivergenceText(out.Detail, maxDivergenceDetailRunes)
	if out.Detail == "" {
		out.Detail = "execution result mismatch"
	}
	return &out
}

func replayDivergenceKind(kind string) string {
	switch kind {
	case "missing_record", "skip_mismatch", "tx_count", "tx_record":
		return kind
	default:
		return "unknown"
	}
}

func boundedDivergenceText(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

func activePersistedVerificationDivergence(st *state.MithrilState, alpenglowMode bool) bool {
	if st == nil {
		return false
	}
	if len(st.ReplayDivergenceEvidence) > 0 {
		return true
	}
	if !alpenglowMode {
		return false
	}
	for _, evidence := range st.AlpenglowEvidence {
		if evidence.Slot > st.LastRootedSlot {
			return true
		}
	}
	return false
}

// normalizeReplayDivergenceEvidence bounds loaded records and retains the
// earliest disputed slots when the persisted collection is over its cap.
func normalizeReplayDivergenceEvidence(st *state.MithrilState) {
	if st == nil || len(st.ReplayDivergenceEvidence) == 0 {
		return
	}

	type evidenceKey struct {
		slot    uint64
		txIndex int
		kind    string
	}
	seen := make(map[evidenceKey]struct{}, len(st.ReplayDivergenceEvidence))
	normalized := make([]state.ReplayDivergenceRecord, 0, len(st.ReplayDivergenceEvidence))
	for _, record := range st.ReplayDivergenceEvidence {
		safe := boundedReplayDivergence(&ReplayDivergence{
			Slot:        record.Slot,
			TxIndex:     record.TxIndex,
			TxSignature: record.TxSignature,
			Kind:        record.Kind,
			Detail:      record.Detail,
		})
		key := evidenceKey{slot: safe.Slot, txIndex: safe.TxIndex, kind: safe.Kind}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, state.ReplayDivergenceRecord{
			Slot:        safe.Slot,
			TxIndex:     safe.TxIndex,
			TxSignature: safe.TxSignature,
			Kind:        safe.Kind,
			Detail:      safe.Detail,
			RecordedAt:  boundedDivergenceText(record.RecordedAt, maxDivergenceRecordedRunes),
		})
	}
	if len(normalized) > maxReplayDivergenceEvidence {
		sort.SliceStable(normalized, func(i, j int) bool { return normalized[i].Slot < normalized[j].Slot })
		normalized = normalized[:maxReplayDivergenceEvidence]
	}
	if !slices.Equal(st.ReplayDivergenceEvidence, normalized) {
		st.ReplayDivergenceEvidence = normalized
	}
}

// recordReplayDivergenceEvidence adds one bounded, deduplicated record. If the
// collection is full, an earlier disputed slot replaces the latest record so
// the persisted promotion floor never moves forward.
func recordReplayDivergenceEvidence(st *state.MithrilState, d *ReplayDivergence) {
	safe := boundedReplayDivergence(d)
	if st == nil || safe == nil {
		return
	}
	normalizeReplayDivergenceEvidence(st)
	for _, evidence := range st.ReplayDivergenceEvidence {
		if evidence.Slot == safe.Slot && evidence.TxIndex == safe.TxIndex && evidence.Kind == safe.Kind {
			return
		}
	}

	record := state.ReplayDivergenceRecord{
		Slot:        safe.Slot,
		TxIndex:     safe.TxIndex,
		TxSignature: safe.TxSignature,
		Kind:        safe.Kind,
		Detail:      safe.Detail,
		RecordedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if len(st.ReplayDivergenceEvidence) < maxReplayDivergenceEvidence {
		st.ReplayDivergenceEvidence = append(st.ReplayDivergenceEvidence, record)
	} else {
		latest := 0
		for i := 1; i < len(st.ReplayDivergenceEvidence); i++ {
			if st.ReplayDivergenceEvidence[i].Slot > st.ReplayDivergenceEvidence[latest].Slot {
				latest = i
			}
		}
		if safe.Slot >= st.ReplayDivergenceEvidence[latest].Slot {
			return
		}
		st.ReplayDivergenceEvidence[latest] = record
	}

	mlog.Log.Errorf("replay divergence evidence recorded for slot %d (%s) — folds blocked at that slot until cleared",
		safe.Slot, safe.Kind)
}
