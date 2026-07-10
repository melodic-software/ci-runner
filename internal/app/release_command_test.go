package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateReleaseCandidateUsesStrictCompatibilityLoader(t *testing.T) {
	manifest := validCompatibilityManifest()
	contents, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "compatibility.json")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := validateReleaseCandidate(path, "1.2.3", "1.2.3")
	if err != nil {
		t.Fatalf("validateReleaseCandidate: %v", err)
	}
	if !result.Valid || result.SourceSHA != manifest.Source.SHA || result.WorkerReference != manifest.WorkerReference() {
		t.Fatalf("result = %#v", result)
	}
}

func TestValidateReleaseCandidateRejectsWrongBinaryAndManifestEvidence(t *testing.T) {
	if _, err := validateReleaseCandidate("unused", "1.2.3", "1.2.4"); err == nil || !strings.Contains(err.Error(), "does not match candidate") {
		t.Fatalf("expected binary mismatch, got %v", err)
	}
	manifest := validCompatibilityManifest()
	manifest.Evidence.WorkerProvenance = "oci://wrong#provenance"
	contents, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "compatibility.json")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateReleaseCandidate(path, "1.2.3", "1.2.3"); err == nil || !strings.Contains(err.Error(), "worker provenance") {
		t.Fatalf("expected evidence mismatch, got %v", err)
	}
}
