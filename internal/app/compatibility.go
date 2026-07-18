package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/melodic-software/ci-runner/internal/scaleset"
)

const (
	compatibilitySchemaVersion = 1
	compatibilityMaximumSize   = 1 << 20
	productionSourceRepository = "melodic-software/ci-runner"
)

var (
	semanticVersionPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?$`)
	shaPattern             = regexp.MustCompile(`^[0-9a-f]{40}$`)
	checksumPattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	digestPattern          = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	workerImagePattern     = regexp.MustCompile(`^ghcr\.io/[a-z0-9._-]+/[a-z0-9._/-]+$`)
)

type CompatibilityManifest struct {
	SchemaVersion  int                       `json:"schemaVersion"`
	CreatedAt      time.Time                 `json:"createdAt"`
	ReleaseVersion string                    `json:"releaseVersion"`
	Source         CompatibilitySource       `json:"source"`
	Controller     CompatibilityController   `json:"controller"`
	Worker         CompatibilityWorker       `json:"worker"`
	Dependencies   CompatibilityDependencies `json:"dependencies"`
	Evidence       CompatibilityEvidence     `json:"evidence"`
}

type CompatibilitySource struct {
	Repository string `json:"repository"`
	SHA        string `json:"sha"`
}

type CompatibilityController struct {
	Version        string `json:"version"`
	WindowsArchive string `json:"windowsArchive"`
	ArchiveDigest  string `json:"archiveDigest"`
}

type CompatibilityWorker struct {
	Image  string `json:"image"`
	Digest string `json:"digest"`
}

type CompatibilityDependencies struct {
	RunnerVersion                 string `json:"runnerVersion"`
	ScaleSetClientVersion         string `json:"scaleSetClientVersion"`
	ScaleSetClientCommit          string `json:"scaleSetClientCommit"`
	GoToolchain                   string `json:"goToolchain"`
	PowerShellVersion             string `json:"powerShellVersion"`
	GHVersion                     string `json:"ghVersion"`
	GHLinuxAMD64ArchiveSHA256     string `json:"ghLinuxAmd64ArchiveSha256"`
	BuildxVersion                 string `json:"buildxVersion"`
	BuildxLinuxAMD64SHA256        string `json:"buildxLinuxAmd64Sha256"`
	BuildKitVersion               string `json:"buildKitVersion"`
	BuildKitDigest                string `json:"buildKitDigest"`
	BuildKitLinuxAMD64Digest      string `json:"buildKitLinuxAmd64Digest"`
	SBOMGeneratorVersion          string `json:"sbomGeneratorVersion"`
	SBOMGeneratorDigest           string `json:"sbomGeneratorDigest"`
	SBOMGeneratorLinuxAMD64Digest string `json:"sbomGeneratorLinuxAmd64Digest"`
}

type CompatibilityEvidence struct {
	Checksums            string `json:"checksums"`
	ControllerSBOM       string `json:"controllerSbom"`
	ControllerProvenance string `json:"controllerProvenance"`
	WorkerSBOM           string `json:"workerSbom"`
	WorkerProvenance     string `json:"workerProvenance"`
}

func LoadCompatibilityManifest(path, expectedControllerVersion string) (CompatibilityManifest, error) {
	if expectedControllerVersion == "" || expectedControllerVersion == "dev" {
		return CompatibilityManifest{}, errors.New("production controller requires an injected non-development version")
	}
	file, err := os.Open(path)
	if err != nil {
		return CompatibilityManifest{}, fmt.Errorf("open compatibility manifest: %w", err)
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, compatibilityMaximumSize+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return CompatibilityManifest{}, fmt.Errorf("read compatibility manifest: %w", err)
	}
	if len(contents) > compatibilityMaximumSize {
		return CompatibilityManifest{}, fmt.Errorf("compatibility manifest exceeds %d bytes", compatibilityMaximumSize)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var manifest CompatibilityManifest
	if err := decoder.Decode(&manifest); err != nil {
		return CompatibilityManifest{}, fmt.Errorf("decode compatibility manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return CompatibilityManifest{}, errors.New("compatibility manifest contains multiple JSON values")
		}
		return CompatibilityManifest{}, fmt.Errorf("decode compatibility manifest trailer: %w", err)
	}
	if err := manifest.Validate(expectedControllerVersion); err != nil {
		return CompatibilityManifest{}, err
	}
	return manifest, nil
}

func (m CompatibilityManifest) Validate(expectedControllerVersion string) error {
	var problems []error
	add := func(ok bool, message string) {
		if !ok {
			problems = append(problems, errors.New(message))
		}
	}
	add(m.SchemaVersion == compatibilitySchemaVersion, fmt.Sprintf("unsupported compatibility schemaVersion %d", m.SchemaVersion))
	add(!m.CreatedAt.IsZero(), "compatibility createdAt is required")
	add(semanticVersionPattern.MatchString(expectedControllerVersion), "injected controller version is not strict SemVer")
	add(m.Controller.Version == expectedControllerVersion, "compatibility controller version does not match this executable")
	add(m.ReleaseVersion == "v"+expectedControllerVersion, "compatibility releaseVersion does not match this executable")
	add(m.Source.Repository == productionSourceRepository, "compatibility source repository is not melodic-software/ci-runner")
	add(shaPattern.MatchString(m.Source.SHA), "compatibility source SHA must be 40 lowercase hexadecimal characters")
	add(m.Controller.WindowsArchive == fmt.Sprintf("ci-runner-v%s-windows-amd64.zip", expectedControllerVersion), "compatibility Windows archive name does not match this executable")
	add(digestPattern.MatchString(m.Controller.ArchiveDigest), "compatibility controller archive digest is invalid")
	add(workerImagePattern.MatchString(m.Worker.Image), "compatibility worker image must be an untagged ghcr.io repository")
	add(digestPattern.MatchString(m.Worker.Digest), "compatibility worker digest is invalid")
	add(semanticVersionPattern.MatchString(m.Dependencies.RunnerVersion), "compatibility runner version is invalid")
	add(m.Dependencies.ScaleSetClientVersion == strings.TrimPrefix(scaleset.OfficialClientVersion, "v"), "compatibility Scale Set Client version does not match the compiled client")
	add(m.Dependencies.ScaleSetClientCommit == scaleset.OfficialClientCommit, "compatibility Scale Set Client commit does not match the compiled client")
	add(m.Dependencies.GoToolchain == runtime.Version(), "compatibility Go toolchain does not match this executable")
	add(semanticVersionPattern.MatchString(m.Dependencies.PowerShellVersion), "compatibility PowerShell version is invalid")
	add(semanticVersionPattern.MatchString(m.Dependencies.GHVersion), "compatibility GitHub CLI version is invalid")
	add(checksumPattern.MatchString(m.Dependencies.GHLinuxAMD64ArchiveSHA256), "compatibility GitHub CLI linux/amd64 archive checksum is invalid")
	add(semanticVersionPattern.MatchString(m.Dependencies.BuildxVersion), "compatibility Buildx version is invalid")
	add(checksumPattern.MatchString(m.Dependencies.BuildxLinuxAMD64SHA256), "compatibility Buildx linux/amd64 checksum is invalid")
	add(semanticVersionPattern.MatchString(m.Dependencies.BuildKitVersion), "compatibility BuildKit version is invalid")
	add(digestPattern.MatchString(m.Dependencies.BuildKitDigest), "compatibility BuildKit manifest digest is invalid")
	add(digestPattern.MatchString(m.Dependencies.BuildKitLinuxAMD64Digest), "compatibility BuildKit linux/amd64 digest is invalid")
	add(semanticVersionPattern.MatchString(m.Dependencies.SBOMGeneratorVersion), "compatibility SBOM generator version is invalid")
	add(digestPattern.MatchString(m.Dependencies.SBOMGeneratorDigest), "compatibility SBOM generator manifest digest is invalid")
	add(digestPattern.MatchString(m.Dependencies.SBOMGeneratorLinuxAMD64Digest), "compatibility SBOM generator linux/amd64 digest is invalid")
	add(m.Evidence.Checksums == "SHA256SUMS", "compatibility checksum evidence must be SHA256SUMS")
	add(m.Evidence.ControllerSBOM == fmt.Sprintf("ci-runner-v%s-windows-amd64.spdx.json", expectedControllerVersion), "compatibility controller SBOM reference does not match this executable")
	add(m.Evidence.ControllerProvenance == "https://github.com/melodic-software/ci-runner/attestations", "compatibility controller provenance reference is invalid")
	workerReference := m.Worker.Image + "@" + m.Worker.Digest
	add(m.Evidence.WorkerSBOM == "oci://"+workerReference+"#sbom", "compatibility worker SBOM reference does not match the worker digest")
	add(m.Evidence.WorkerProvenance == "oci://"+workerReference+"#provenance", "compatibility worker provenance reference does not match the worker digest")
	if len(problems) > 0 {
		return fmt.Errorf("invalid compatibility manifest: %w", errors.Join(problems...))
	}
	return nil
}

func (m CompatibilityManifest) WorkerReference() string {
	return m.Worker.Image + "@" + m.Worker.Digest
}
