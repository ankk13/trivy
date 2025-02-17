package local

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/wire"
	"golang.org/x/xerrors"

	"github.com/aquasecurity/fanal/analyzer"
	_ "github.com/aquasecurity/fanal/analyzer/all"
	"github.com/aquasecurity/fanal/applier"
	ftypes "github.com/aquasecurity/fanal/types"
	"github.com/aquasecurity/trivy/pkg/detector/library"
	ospkgDetector "github.com/aquasecurity/trivy/pkg/detector/ospkg"
	"github.com/aquasecurity/trivy/pkg/log"
	"github.com/aquasecurity/trivy/pkg/report"
	"github.com/aquasecurity/trivy/pkg/types"
	"github.com/aquasecurity/trivy/pkg/utils"
)

// SuperSet binds dependencies for Local scan
var SuperSet = wire.NewSet(
	applier.NewApplier,
	wire.Bind(new(Applier), new(applier.Applier)),
	ospkgDetector.SuperSet,
	wire.Bind(new(OspkgDetector), new(ospkgDetector.Detector)),
	NewScanner,
)

// Applier defines operation to scan image layers
type Applier interface {
	ApplyLayers(artifactID string, blobIDs []string) (detail ftypes.ArtifactDetail, err error)
}

// OspkgDetector defines operation to detect OS vulnerabilities
type OspkgDetector interface {
	Detect(imageName, osFamily, osName string, created time.Time, pkgs []ftypes.Package) (detectedVulns []types.DetectedVulnerability, eosl bool, err error)
}

// Scanner implements the OspkgDetector and LibraryDetector
type Scanner struct {
	applier       Applier
	ospkgDetector OspkgDetector
}

// NewScanner is the factory method for Scanner
func NewScanner(applier Applier, ospkgDetector OspkgDetector) Scanner {
	return Scanner{applier: applier, ospkgDetector: ospkgDetector}
}

// Scan scans the artifact and return results.
func (s Scanner) Scan(target, versionedArtifactID string, versionedBlobIDs []string, options types.ScanOptions) (
	report.Results, *ftypes.OS, bool, error) {
	artifactDetail, err := s.applier.ApplyLayers(versionedArtifactID, versionedBlobIDs)
	switch {
	case errors.Is(err, analyzer.ErrUnknownOS):
		log.Logger.Debug("OS is not detected and vulnerabilities in OS packages are not detected.")
	case errors.Is(err, analyzer.ErrNoPkgsDetected):
		log.Logger.Warn("No OS package is detected. Make sure you haven't deleted any files that contain information about the installed packages.")
		log.Logger.Warn(`e.g. files under "/lib/apk/db/", "/var/lib/dpkg/" and "/var/lib/rpm"`)
	case err != nil:
		return nil, nil, false, xerrors.Errorf("failed to apply layers: %w", err)
	}

	var eosl bool
	var results report.Results

	if options.AnalyzeOnly {
		return s.onlyResources(results, artifactDetail, target, options), artifactDetail.OS, eosl, nil
	}

	// Scan OS packages and programming language dependencies
	if utils.StringInSlice(types.SecurityCheckVulnerability, options.SecurityChecks) {
		var vulnResults report.Results
		vulnResults, eosl, err = s.checkVulnerabilities(target, artifactDetail, options)
		if err != nil {
			return nil, nil, false, xerrors.Errorf("failed to detect vulnerabilities: %w", err)
		}
		results = append(results, vulnResults...)
	}

	return results, artifactDetail.OS, eosl, nil
}

func (s Scanner) onlyResources(results report.Results, artifactDetail ftypes.ArtifactDetail, target string, options types.ScanOptions) report.Results {
	// collect os packages
	if artifactDetail.OS != nil {
		results = append(results, report.Result{
			Target:   fmt.Sprintf("%s (%s %s)", target, artifactDetail.OS.Family, artifactDetail.OS.Name),
			Type:     artifactDetail.OS.Family,
			Packages: artifactDetail.Packages,
		})
	}
	// collect libraries
	for _, app := range artifactDetail.Applications {
		if len(app.Libraries) == 0 {
			continue
		}
		if skipped(app.FilePath, options.SkipFiles, options.SkipDirs) {
			continue
		}

		var pkgs []ftypes.Package
		for _, lib := range app.Libraries {
			pkgs = append(pkgs, ftypes.Package{
				Name:    lib.Library.Name,
				Version: lib.Library.Version,
				Layer:   lib.Layer,
			})
		}
		results = append(results, report.Result{
			Target:   app.FilePath,
			Type:     app.Type,
			Packages: pkgs,
		})
	}
	return results
}

func (s Scanner) checkVulnerabilities(target string, detail ftypes.ArtifactDetail, options types.ScanOptions) (
	report.Results, bool, error) {
	var eosl bool
	var results report.Results

	if utils.StringInSlice(types.VulnTypeOS, options.VulnType) {
		result, detectedEosl, err := s.scanOSPkgs(target, detail, options)
		if err != nil {
			return nil, false, xerrors.Errorf("unable to scan OS packages: %w", err)
		} else if result != nil {
			results = append(results, *result)
		}
		eosl = detectedEosl
	}

	if utils.StringInSlice(types.VulnTypeLibrary, options.VulnType) {
		libResults, err := s.scanLibrary(detail.Applications, options)
		if err != nil {
			return nil, false, xerrors.Errorf("failed to scan application libraries: %w", err)
		}
		results = append(results, libResults...)
	}

	return results, eosl, nil
}

func (s Scanner) scanOSPkgs(target string, detail ftypes.ArtifactDetail, options types.ScanOptions) (
	*report.Result, bool, error) {
	if detail.OS == nil {
		log.Logger.Infof("Detected OS: unknown")
		return nil, false, nil
	}
	log.Logger.Infof("Detected OS: %s", detail.OS.Family)

	pkgs := detail.Packages
	if options.ScanRemovedPackages {
		pkgs = mergePkgs(pkgs, detail.HistoryPackages)
	}

	result, eosl, err := s.detectVulnsInOSPkgs(target, detail.OS.Family, detail.OS.Name, pkgs)
	if err != nil {
		return nil, false, xerrors.Errorf("failed to scan OS packages: %w", err)
	} else if result == nil {
		return nil, eosl, nil
	}

	if options.ListAllPackages {
		sort.Slice(pkgs, func(i, j int) bool {
			return strings.Compare(pkgs[i].Name, pkgs[j].Name) <= 0
		})
		result.Packages = pkgs
	}

	return result, eosl, nil
}

func (s Scanner) detectVulnsInOSPkgs(target, osFamily, osName string, pkgs []ftypes.Package) (*report.Result, bool, error) {
	if osFamily == "" {
		return nil, false, nil
	}
	vulns, eosl, err := s.ospkgDetector.Detect("", osFamily, osName, time.Time{}, pkgs)
	if err == ospkgDetector.ErrUnsupportedOS {
		return nil, false, nil
	} else if err != nil {
		return nil, false, xerrors.Errorf("failed vulnerability detection of OS packages: %w", err)
	}

	artifactDetail := fmt.Sprintf("%s (%s %s)", target, osFamily, osName)
	result := &report.Result{
		Target:          artifactDetail,
		Vulnerabilities: vulns,
		Type:            osFamily,
	}
	return result, eosl, nil
}

func (s Scanner) scanLibrary(apps []ftypes.Application, options types.ScanOptions) (report.Results, error) {
	log.Logger.Infof("Number of PL dependency files: %d", len(apps))
	if len(apps) == 0 {
		return nil, nil
	}

	var results report.Results
	printedTypes := map[string]struct{}{}
	for _, app := range apps {
		if len(app.Libraries) == 0 {
			continue
		}
		if skipped(app.FilePath, options.SkipFiles, options.SkipDirs) {
			continue
		}

		// Prevent the same log messages from being displayed many times for the same type.
		if _, ok := printedTypes[app.Type]; !ok {
			log.Logger.Infof("Detecting %s vulnerabilities...", app.Type)
			printedTypes[app.Type] = struct{}{}
		}

		log.Logger.Debugf("Detecting library vulnerabilities, type: %s, path: %s", app.Type, app.FilePath)
		vulns, err := library.Detect(app.Type, app.Libraries)
		if err != nil {
			return nil, xerrors.Errorf("failed vulnerability detection of libraries: %w", err)
		}

		libReport := report.Result{
			Target:          app.FilePath,
			Vulnerabilities: vulns,
			Type:            app.Type,
		}
		if options.ListAllPackages {
			var pkgs []ftypes.Package
			for _, lib := range app.Libraries {
				pkgs = append(pkgs, ftypes.Package{
					Name:    lib.Library.Name,
					Version: lib.Library.Version,
					Layer:   lib.Layer,
				})
			}
			sort.Slice(pkgs, func(i, j int) bool {
				return strings.Compare(pkgs[i].Name, pkgs[j].Name) <= 0
			})
			libReport.Packages = pkgs
		}
		results = append(results, libReport)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Target < results[j].Target
	})
	return results, nil
}

func skipped(filePath string, skipFiles, skipDirs []string) bool {
	filePath = strings.TrimLeft(filepath.Clean(filePath), string(os.PathSeparator))
	for _, skipFile := range skipFiles {
		skipFile = strings.TrimLeft(filepath.Clean(skipFile), string(os.PathSeparator))
		if filePath == skipFile {
			return true
		}
	}

	for _, skipDir := range skipDirs {
		skipDir = strings.TrimLeft(filepath.Clean(skipDir), string(os.PathSeparator))
		rel, err := filepath.Rel(skipDir, filePath)
		if err != nil {
			log.Logger.Warnf("Unexpected error while skipping directories: %s", err)
			return false
		}
		if !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	return false
}

func mergePkgs(pkgs, pkgsFromCommands []ftypes.Package) []ftypes.Package {
	// pkg has priority over pkgsFromCommands
	uniqPkgs := map[string]struct{}{}
	for _, pkg := range pkgs {
		uniqPkgs[pkg.Name] = struct{}{}
	}
	for _, pkg := range pkgsFromCommands {
		if _, ok := uniqPkgs[pkg.Name]; ok {
			continue
		}
		pkgs = append(pkgs, pkg)
	}
	return pkgs
}
