package gossip

import (
	"fmt"
	"net"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
)

// ResolveShredVersion uses the gossip entrypoint shred version when it disagrees
// with the configured value. Entrypoint IP echo is authoritative for the cluster
// reachable at that address.
func ResolveShredVersion(entrypoint *net.UDPAddr, configured uint16, timeout time.Duration) (uint16, error) {
	if entrypoint == nil {
		if configured == 0 {
			return 0, fmt.Errorf("gossip entrypoint is required to discover shred version")
		}
		return configured, nil
	}
	echo, err := QueryEntrypoint(entrypoint, timeout)
	if err != nil {
		if configured == 0 {
			return 0, fmt.Errorf("query gossip entrypoint %s: %w", entrypoint.String(), err)
		}
		return configured, nil
	}
	if configured == 0 {
		if echo.ShredVersion == 0 {
			return 0, fmt.Errorf("entrypoint %s did not return a shred version", entrypoint.String())
		}
		return echo.ShredVersion, nil
	}
	if echo.ShredVersion != 0 && configured != echo.ShredVersion {
		mlog.Log.Warnf("configured shred version %d differs from gossip entrypoint %s (%d); using entrypoint shred version",
			configured, entrypoint.String(), echo.ShredVersion)
		return echo.ShredVersion, nil
	}
	return configured, nil
}
