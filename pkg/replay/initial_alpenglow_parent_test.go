package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestInitialAlpenglowParentBlockID(t *testing.T) {
	manifestID := solana.Hash{1}
	resumeID := solana.Hash{2}
	tests := []struct {
		name      string
		state     *state.MithrilState
		resume    *ResumeState
		want      solana.Hash
		wantError string
	}{
		{name: "fresh snapshot", state: &state.MithrilState{ManifestParentAlpenglowBlockID: manifestID.String()}, want: manifestID},
		{name: "resume overrides snapshot", state: &state.MithrilState{ManifestParentAlpenglowBlockID: manifestID.String()}, resume: &ResumeState{ParentAlpenglowBlockID: resumeID, HasParentAlpenglowBlockID: true}, want: resumeID},
		{name: "missing snapshot identity", state: &state.MithrilState{}, wantError: "missing"},
		{name: "invalid snapshot identity", state: &state.MithrilState{ManifestParentAlpenglowBlockID: "not-base58"}, wantError: "decode"},
		{name: "zero snapshot identity", state: &state.MithrilState{ManifestParentAlpenglowBlockID: (solana.Hash{}).String()}, wantError: "all-zero"},
		{name: "missing resume identity", state: &state.MithrilState{ManifestParentAlpenglowBlockID: manifestID.String()}, resume: &ResumeState{}, wantError: "missing"},
		{name: "zero resume identity", resume: &ResumeState{HasParentAlpenglowBlockID: true}, wantError: "all-zero"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := InitialAlpenglowParentBlockID(test.state, test.resume)
			if test.wantError != "" {
				require.ErrorContains(t, err, test.wantError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}
