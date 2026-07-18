package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/scaleset"
)

func validCompatibilityManifest() CompatibilityManifest {
	return CompatibilityManifest{
		SchemaVersion:  1,
		CreatedAt:      time.Date(2026, 7, 10, 1, 2, 3, 0, time.UTC),
		ReleaseVersion: "v1.2.3",
		Source:         CompatibilitySource{Repository: productionSourceRepository, SHA: strings.Repeat("a", 40)},
		Controller: CompatibilityController{
			Version: "1.2.3", WindowsArchive: "ci-runner-v1.2.3-windows-amd64.zip",
			ArchiveDigest: "sha256:" + strings.Repeat("b", 64),
		},
		Worker: CompatibilityWorker{Image: "ghcr.io/melodic-software/ci-runner", Digest: "sha256:" + strings.Repeat("c", 64)},
		Dependencies: CompatibilityDependencies{
			RunnerVersion: "2.335.1", ScaleSetClientVersion: strings.TrimPrefix(scaleset.OfficialClientVersion, "v"),
			ScaleSetClientCommit: scaleset.OfficialClientCommit, GoToolchain: runtime.Version(), PowerShellVersion: "7.5.2",
			GHVersion: "2.95.0", GHLinuxAMD64ArchiveSHA256: strings.Repeat("9", 64),
			BuildxVersion: "0.35.0", BuildxLinuxAMD64SHA256: strings.Repeat("d", 64),
			BuildKitVersion: "0.31.1", BuildKitDigest: "sha256:" + strings.Repeat("e", 64),
			BuildKitLinuxAMD64Digest: "sha256:" + strings.Repeat("f", 64),
			SBOMGeneratorVersion:     "1.11.0", SBOMGeneratorDigest: "sha256:" + strings.Repeat("1", 64),
			SBOMGeneratorLinuxAMD64Digest: "sha256:" + strings.Repeat("2", 64),
		},
		Evidence: CompatibilityEvidence{
			Checksums: "SHA256SUMS", ControllerSBOM: "ci-runner-v1.2.3-windows-amd64.spdx.json", ControllerProvenance: "https://github.com/melodic-software/ci-runner/attestations",
			WorkerSBOM:       "oci://ghcr.io/melodic-software/ci-runner@sha256:" + strings.Repeat("c", 64) + "#sbom",
			WorkerProvenance: "oci://ghcr.io/melodic-software/ci-runner@sha256:" + strings.Repeat("c", 64) + "#provenance",
		},
	}
}

func TestCompatibilityManifestBindsControllerAndWorkerPair(t *testing.T) {
	manifest := validCompatibilityManifest()
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "compatibility.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCompatibilityManifest(path, "1.2.3")
	if err != nil {
		t.Fatalf("LoadCompatibilityManifest: %v", err)
	}
	if got := loaded.WorkerReference(); got != manifest.Worker.Image+"@"+manifest.Worker.Digest {
		t.Fatalf("worker reference = %q", got)
	}
}

func TestCompatibilityManifestRejectsUnknownFieldsAndVersionDrift(t *testing.T) {
	manifest := validCompatibilityManifest()
	if err := manifest.Validate("9.9.9"); err == nil {
		t.Fatal("expected compiled-version mismatch")
	}
	path := filepath.Join(t.TempDir(), "compatibility.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"unexpected":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCompatibilityManifest(path, "1.2.3"); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown-field rejection, got %v", err)
	}
}

func TestCompatibilityManifestRejectsDevelopmentBuild(t *testing.T) {
	if _, err := LoadCompatibilityManifest("unused", "dev"); err == nil {
		t.Fatal("development controller must not start as a production controller")
	}
}

func TestCompatibilityManifestRejectsInvalidBuilderEvidence(t *testing.T) {
	manifest := validCompatibilityManifest()
	manifest.Dependencies.BuildxVersion = "latest"
	manifest.Dependencies.BuildKitLinuxAMD64Digest = "sha256:mutable"
	manifest.Dependencies.SBOMGeneratorDigest = strings.Repeat("a", 64)

	err := manifest.Validate("1.2.3")
	if err == nil {
		t.Fatal("expected invalid builder evidence to be rejected")
	}
	for _, message := range []string{
		"compatibility Buildx version is invalid",
		"compatibility BuildKit linux/amd64 digest is invalid",
		"compatibility SBOM generator manifest digest is invalid",
	} {
		if !strings.Contains(err.Error(), message) {
			t.Fatalf("expected %q in %v", message, err)
		}
	}
}
