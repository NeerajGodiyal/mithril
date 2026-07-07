package sealevel

import "testing"

// The sysvar read seam resolves to a slot's branch-scoped cache when set, and to the
// shared process-global cache otherwise. This is what lets sysvars become per-branch
// without touching the read call sites; today the branch cache is always nil (single
// branch), so every read routes to the global — a strict no-op.
func TestSysvarCacheForResolvesBranchThenGlobal(t *testing.T) {
	branch := &sysvarCache{}

	cases := []struct {
		name string
		slot *SlotCtx
		want *sysvarCache
	}{
		{"nil slot -> global", nil, &SysvarCache},
		{"slot without branch cache -> global", &SlotCtx{}, &SysvarCache},
		{"slot with branch cache -> branch", &SlotCtx{sysvars: branch}, branch},
	}
	for _, c := range cases {
		if got := sysvarCacheForSlot(c.slot); got != c.want {
			t.Fatalf("%s: sysvarCacheForSlot = %p, want %p", c.name, got, c.want)
		}
	}

	if got := sysvarCacheFor(nil); got != &SysvarCache {
		t.Fatalf("nil execCtx must resolve to the global cache")
	}
	if got := sysvarCacheFor(&ExecutionCtx{SlotCtx: &SlotCtx{sysvars: branch}}); got != branch {
		t.Fatalf("execCtx with a branch cache must resolve to it")
	}
}
