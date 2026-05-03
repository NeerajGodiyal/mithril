package setupcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestHandleSelect_ScrLightbringerQuiet verifies that picking an option on
// the new quiet-mode screen sets m.lbQuiet correctly.
func TestHandleSelect_ScrLightbringerQuiet_True(t *testing.T) {
	m := setupModel{screen: scrLightbringerQuiet, mode: "full"}
	out, _ := m.handleSelect("true")
	model := out.(setupModel)
	assert.True(t, model.lbQuiet, "picking 'true' should set lbQuiet=true")
}

func TestHandleSelect_ScrLightbringerQuiet_False(t *testing.T) {
	m := setupModel{screen: scrLightbringerQuiet, mode: "full", lbQuiet: true}
	out, _ := m.handleSelect("false")
	model := out.(setupModel)
	assert.False(t, model.lbQuiet, "picking 'false' should set lbQuiet=false")
}

// TestHandleSelect_DisableLB_ResetsQuiet verifies that picking "disable" on
// Lightbringer clears m.lbQuiet so a later re-enable starts clean.
func TestHandleSelect_DisableLB_ResetsQuiet(t *testing.T) {
	m := setupModel{
		screen:   scrLightbringer,
		mode:     "full",
		enableLB: true,
		lbQuiet:  true, // user previously picked Quiet
	}
	out, _ := m.handleSelect("disable")
	model := out.(setupModel)
	assert.False(t, model.enableLB, "disable should set enableLB=false")
	assert.False(t, model.lbQuiet, "disable should reset lbQuiet=false to avoid stale state on re-enable")
}

// TestHandleSelect_EnableLB_DoesNotChangeQuiet ensures picking "enable"
// preserves whatever quiet value the user had (so toggling enable->enable
// doesn't reset it).
func TestHandleSelect_EnableLB_PreservesQuiet(t *testing.T) {
	m := setupModel{
		screen:   scrLightbringer,
		mode:     "full",
		enableLB: false,
		lbQuiet:  true, // shouldn't be true if enableLB was false, but test the invariant
	}
	out, _ := m.handleSelect("enable")
	model := out.(setupModel)
	assert.True(t, model.enableLB)
	assert.True(t, model.lbQuiet, "enable should not modify lbQuiet")
}

// TestNewSetupModel_StoragePathsPopulated verifies the model picks up storage
// defaults from config.DefaultStoragePaths() at construction.
func TestNewSetupModel_StoragePathsPopulated(t *testing.T) {
	m := newSetupModel()
	assert.NotEmpty(t, m.accountsPath)
	assert.NotEmpty(t, m.snapshotsPath)
	assert.NotEmpty(t, m.logsPath)
	assert.NotEmpty(t, m.shredstorePath)
	// Specific paths depend on the host's /mnt/mithril-accounts writability;
	// the all-or-nothing invariant is covered by pkg/config tests.
}
