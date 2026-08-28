package configcmd

import (
	"strings"
	"testing"
)

func TestGenerateStarterConfigListsRootedEvents(t *testing.T) {
	if got := generateStarterConfig(false); !strings.Contains(got, "rooted_events = false") {
		t.Fatal("starter config omits the rooted-event feed opt-in")
	}
}
