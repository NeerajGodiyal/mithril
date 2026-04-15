//go:build linux

package setupcmd

import (
	"fmt"
	"os/exec"
	"strings"
)

// DiskInfo represents a detected disk/mount
type DiskInfo struct {
	Path      string
	Device    string
	SizeGB    string
	FreeGB    string
	FSType    string
}

// DetectDisks returns mounted filesystems with free space info.
func DetectDisks() []DiskInfo {
	out, err := exec.Command("df", "-BG", "--output=target,source,size,avail,fstype").Output()
	if err != nil {
		return nil
	}

	var disks []DiskInfo
	lines := strings.Split(string(out), "\n")
	for _, line := range lines[1:] { // skip header
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mount := fields[0]
		// Skip system mounts
		if mount == "/" || strings.HasPrefix(mount, "/boot") || strings.HasPrefix(mount, "/snap") ||
			strings.HasPrefix(mount, "/sys") || strings.HasPrefix(mount, "/proc") || strings.HasPrefix(mount, "/dev") ||
			strings.HasPrefix(mount, "/run") {
			continue
		}
		disks = append(disks, DiskInfo{
			Path:   mount,
			Device: fields[1],
			SizeGB: strings.TrimSuffix(fields[2], "G"),
			FreeGB: strings.TrimSuffix(fields[3], "G"),
			FSType: fields[4],
		})
	}

	return disks
}

// FormatDiskOption returns a display string for a disk.
func (d DiskInfo) FormatDiskOption() string {
	return fmt.Sprintf("%s (%s GB free / %s GB total) — %s", d.Path, d.FreeGB, d.SizeGB, d.Device)
}
