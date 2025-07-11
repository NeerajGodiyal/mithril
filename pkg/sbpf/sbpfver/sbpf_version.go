package sbpfver

import "github.com/Overclock-Validator/mithril/pkg/features"

const (
	SbpfVersionV0 = iota
	SbpfVersionV1
	SbpfVersionV2
	SbpfVersionV3
)

type SbpfVersion struct {
	Version uint32
}

func (ver *SbpfVersion) DynamicStackFrames() bool {
	return ver.Version >= SbpfVersionV1
}

func (ver *SbpfVersion) RejectRodataStackOverlap() bool {
	return ver.Version != SbpfVersionV0
}

func (ver *SbpfVersion) EnableElfVAddr() bool {
	return ver.Version != SbpfVersionV0
}

func GetMinAndMaxSbpfVersions(f *features.Features) (uint32, uint32) {
	disableSbpfV0 := f.IsActive(features.DisableSbpfV0Execution)
	reenableSbpfV0 := f.IsActive(features.ReenableSbpfV0Execution)
	enableSbpfV0 := !disableSbpfV0 || reenableSbpfV0
	enableSbpfV1 := f.IsActive(features.EnableSbpfV1DeploymentAndExecution)
	enableSbpfV2 := f.IsActive(features.EnableSbpfV2DeploymentAndExecution)
	enableSbpfV3 := f.IsActive(features.EnableSbpfV3DeploymentAndExecution)

	var maxVer, minVer uint32

	// minimum version
	if enableSbpfV0 {
		minVer = SbpfVersionV0
	} else {
		minVer = SbpfVersionV3
	}

	// max version
	if enableSbpfV3 {
		maxVer = SbpfVersionV3
	} else if enableSbpfV2 {
		maxVer = SbpfVersionV2
	} else if enableSbpfV1 {
		maxVer = SbpfVersionV1
	} else {
		maxVer = SbpfVersionV0
	}

	return minVer, maxVer
}
