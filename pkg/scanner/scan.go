package scanner

import (
	"context"

	"github.com/google/wire"
	"golang.org/x/xerrors"

	"github.com/aquasecurity/fanal/artifact"
	aimage "github.com/aquasecurity/fanal/artifact/image"
	flocal "github.com/aquasecurity/fanal/artifact/local"
	"github.com/aquasecurity/fanal/artifact/remote"
	"github.com/aquasecurity/fanal/image"
	ftypes "github.com/aquasecurity/fanal/types"
	"github.com/aquasecurity/trivy/pkg/log"
	"github.com/aquasecurity/trivy/pkg/report"
	"github.com/aquasecurity/trivy/pkg/rpc/client"
	"github.com/aquasecurity/trivy/pkg/scanner/local"
	"github.com/aquasecurity/trivy/pkg/types"
)

// StandaloneSuperSet is used in the standalone mode
var StandaloneSuperSet = wire.NewSet(
	local.SuperSet,
	wire.Bind(new(Driver), new(local.Scanner)),
	NewScanner,
)

// StandaloneDockerSet binds docker dependencies
var StandaloneDockerSet = wire.NewSet(
	types.GetDockerOption,
	image.NewDockerImage,
	aimage.NewArtifact,
	StandaloneSuperSet,
)

// StandaloneArchiveSet binds archive scan dependencies
var StandaloneArchiveSet = wire.NewSet(
	image.NewArchiveImage,
	aimage.NewArtifact,
	StandaloneSuperSet,
)

// StandaloneFilesystemSet binds filesystem dependencies
var StandaloneFilesystemSet = wire.NewSet(
	flocal.NewArtifact,
	StandaloneSuperSet,
)

// StandaloneRepositorySet binds repository dependencies
var StandaloneRepositorySet = wire.NewSet(
	remote.NewArtifact,
	StandaloneSuperSet,
)

// RemoteSuperSet is used in the client mode
var RemoteSuperSet = wire.NewSet(
	aimage.NewArtifact,
	client.SuperSet,
	wire.Bind(new(Driver), new(client.Scanner)),
	NewScanner,
)

// RemoteDockerSet binds remote docker dependencies
var RemoteDockerSet = wire.NewSet(
	types.GetDockerOption,
	image.NewDockerImage,
	RemoteSuperSet,
)

// RemoteArchiveSet binds remote archive dependencies
var RemoteArchiveSet = wire.NewSet(
	image.NewArchiveImage,
	RemoteSuperSet,
)

// Scanner implements the Artifact and Driver operations
type Scanner struct {
	driver        Driver
	artifact      artifact.Artifact
	customDrivers []Driver
}

// Driver defines operations of scanner
type Driver interface {
	Scan(target string, artifactKey string, blobKeys []string, options types.ScanOptions) (
		results report.Results, osFound *ftypes.OS, err error)
	GetCache() ftypes.CacheType
}

// NewScanner is the factory method of Scanner
func NewScanner(driver Driver, customdrivers []Driver, ar artifact.Artifact) Scanner {
	return Scanner{drivers: []Driver{driver}, artifact: ar}
}

// ScanArtifact scans the artifacts and returns results
func (s Scanner) ScanArtifact(ctx context.Context, options types.ScanOptions) (report.Report, error) {
	artifactInfo, err := s.artifact.Inspect(ctx)
	if err != nil {
		return report.Report{}, xerrors.Errorf("failed analysis: %w", err)
	}
	results := report.Results{}
	var os *ftypes.OS
	for _, driver := range s.drivers {
		driverResults, osFound, err := driver.Scan(artifactInfo.Name, artifactInfo.ID, artifactInfo.BlobIDs[driver.GetCache()], options)
		if err != nil {
			return report.Report{}, xerrors.Errorf("scan failed: %w", err)
		}
		if osFound != nil {
			os = osFound // Assuming only 1 driver will return operating System
			if osFound.Eosl {
				log.Logger.Warnf("This OS version is no longer supported by the distribution: %s %s", osFound.Family, osFound.Name)
				log.Logger.Warnf("The vulnerability detection may be insufficient because security updates are not provided")
			}
		}

		results = append(results, driverResults...)
	}

	// Layer makes sense only when scanning container images
	if artifactInfo.Type != ftypes.ArtifactContainerImage {
		removeLayer(results)
	}

	return report.Report{
		SchemaVersion: report.SchemaVersion,
		ArtifactName:  artifactInfo.Name,
		ArtifactType:  artifactInfo.Type,
		Metadata: report.Metadata{
			OS:          os,
			ImageID:     artifactInfo.ImageMetadata.ID,
			DiffIDs:     artifactInfo.ImageMetadata.DiffIDs,
			RepoTags:    artifactInfo.ImageMetadata.RepoTags,
			RepoDigests: artifactInfo.ImageMetadata.RepoDigests,
			ImageConfig: artifactInfo.ImageMetadata.ConfigFile,
		},
		Results: results,
	}, nil
}

func removeLayer(results report.Results) {
	for i := range results {
		result := results[i]

		for j := range result.Packages {
			result.Packages[j].Layer = ftypes.Layer{}
		}
		for j := range result.Vulnerabilities {
			result.Vulnerabilities[j].Layer = ftypes.Layer{}
		}
		for j := range result.Misconfigurations {
			result.Misconfigurations[j].Layer = ftypes.Layer{}
		}
	}
}
