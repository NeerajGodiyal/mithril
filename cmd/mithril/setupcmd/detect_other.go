//go:build !linux

package setupcmd

// DiskInfo represents a detected disk/mount
type DiskInfo struct {
	Path   string
	Device string
	SizeGB string
	FreeGB string
	FSType string
}

// DetectDisks returns nil on non-Linux platforms.
func DetectDisks() []DiskInfo {
	return nil
}

// FormatDiskOption returns a display string for a disk.
func (d DiskInfo) FormatDiskOption() string {
	return d.Path
}
