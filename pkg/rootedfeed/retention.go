package rootedfeed

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/rootedevents"
	"github.com/Overclock-Validator/mithril/pkg/state"
)

// Retainer keeps the event sidecars needed by the current durable state and
// AccountsDB rewind horizon. It is not safe for concurrent use.
type Retainer struct {
	root     string
	follower *Follower
}

// NewRetainer prepares one cached retention selector. A horizon of H keeps the
// H committed batches plus the boundary below them.
func NewRetainer(accountsDBRoot string, horizon uint64) (*Retainer, error) {
	retain := horizon + 1
	if retain == 0 {
		return nil, errors.New("rooted-event retention horizon is too large")
	}
	follower, err := NewFollower(accountsDBRoot, retain)
	if err != nil {
		return nil, err
	}
	return &Retainer{root: accountsDBRoot, follower: follower}, nil
}

// Cleanup removes only generated files that no validated live or parked fold
// manifest selects. The current reference is retained independently so state
// recovery cannot race retention bookkeeping.
func (r *Retainer) Cleanup(current *state.RootedEventBatchRef) ([]string, error) {
	if r == nil || r.follower == nil {
		return nil, errors.New("rooted-event retainer is not initialized")
	}
	keep := make(map[string]*state.RootedEventBatchRef)
	if current != nil {
		if err := addRetainedRef(keep, current); err != nil {
			return nil, fmt.Errorf("current rooted-event reference: %w", err)
		}
	}
	selections, err := r.follower.retainedSelections()
	if err != nil {
		return nil, err
	}
	for _, selection := range selections {
		if selection.ref == nil {
			continue
		}
		if err := addRetainedRef(keep, selection.ref); err != nil {
			return nil, fmt.Errorf("retained rooted-event batch %d: %w", selection.sequence, err)
		}
	}

	accounts, err := accountsDir(r.root)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(accounts)
	if err != nil {
		return nil, fmt.Errorf("scan parked rooted-event manifests: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".manifest.rewound") {
			continue
		}
		path := filepath.Join(accounts, entry.Name())
		manifest, err := accountsdb.ReadSegmentManifestContextVerified(path)
		if err != nil {
			return nil, fmt.Errorf("verify parked rooted-event manifest %s: %w", path, err)
		}
		if manifest.Kind != accountsdb.ManifestKindFold {
			return nil, fmt.Errorf("parked rooted-event manifest %s is not a fold manifest", path)
		}
		ctx, err := decodeManifestContext(manifest)
		if err != nil {
			return nil, err
		}
		if ctx.RootedEventBatch == nil {
			continue
		}
		if err := validateManifestRef(manifest, ctx.RootedEventBatch); err != nil {
			return nil, err
		}
		if err := addRetainedRef(keep, ctx.RootedEventBatch); err != nil {
			return nil, err
		}
	}

	refs := make([]*state.RootedEventBatchRef, 0, len(keep))
	for _, ref := range keep {
		refs = append(refs, ref)
	}
	removed, err := cleanupManifestHeadTemps(r.root)
	if err != nil {
		return nil, err
	}
	sidecars, err := rootedevents.CleanupSidecars(r.root, refs)
	return append(removed, sidecars...), err
}

func addRetainedRef(keep map[string]*state.RootedEventBatchRef, ref *state.RootedEventBatchRef) error {
	if err := rootedevents.ValidateSidecarRef(ref); err != nil {
		return err
	}
	copyRef := *ref
	keep[copyRef.File] = &copyRef
	return nil
}

func cleanupManifestHeadTemps(accountsDBRoot string) ([]string, error) {
	accounts, err := accountsDir(accountsDBRoot)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(filepath.Dir(accounts), rootedevents.SidecarDirectory)
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat rooted-event directory for manifest-head cleanup: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("rooted-event path for manifest-head cleanup is not a real directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read rooted-event directory for manifest-head cleanup: %w", err)
	}
	var removed []string
	for _, entry := range entries {
		name := entry.Name()
		isTemp := strings.HasPrefix(name, ".manifest-head-") && strings.HasSuffix(name, ".tmp")
		if entry.IsDir() || !isTemp {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return removed, fmt.Errorf("remove rooted-event manifest-head file %q: %w", name, err)
		}
		removed = append(removed, path)
	}
	if len(removed) == 0 {
		return nil, nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return removed, fmt.Errorf("open rooted-event directory after manifest-head cleanup: %w", err)
	}
	err = d.Sync()
	closeErr := d.Close()
	if err != nil {
		return removed, fmt.Errorf("sync rooted-event directory after manifest-head cleanup: %w", err)
	}
	return removed, closeErr
}
