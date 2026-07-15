package app

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/melodic-software/ci-runner/internal/buildinfo"
)

type releaseValidationResult struct {
	SchemaVersion     int    `json:"schemaVersion"`
	Valid             bool   `json:"valid"`
	Error             string `json:"error,omitempty"`
	ControllerVersion string `json:"controllerVersion,omitempty"`
	ReleaseVersion    string `json:"releaseVersion,omitempty"`
	SourceSHA         string `json:"sourceSha,omitempty"`
	WorkerReference   string `json:"workerReference,omitempty"`
}

func runReleaseCommand(args []string, out, errOut io.Writer) int {
	if len(args) == 0 || args[0] != "validate" {
		writeln(errOut, "usage: ci-runner release validate --manifest ABSOLUTE_PATH --version VERSION")
		return ExitUsage
	}
	flags := flag.NewFlagSet("release validate", flag.ContinueOnError)
	flags.SetOutput(errOut)
	manifestPath := flags.String("manifest", "", "absolute compatibility manifest path")
	version := flags.String("version", "", "expected candidate executable version")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || !filepath.IsAbs(*manifestPath) || strings.TrimSpace(*version) == "" {
		if err == nil {
			writeln(errOut, "--manifest must be absolute and --version is required")
		}
		return ExitUsage
	}
	result, err := validateReleaseCandidate(*manifestPath, *version, buildinfo.Version)
	if err != nil {
		result.Error = err.Error()
		_ = json.NewEncoder(out).Encode(result)
		return ExitInvalidConfig
	}
	if err := json.NewEncoder(out).Encode(result); err != nil {
		writef(errOut, "write release validation result: %v\n", err)
		return ExitRuntime
	}
	return ExitOK
}

func validateReleaseCandidate(manifestPath, requestedVersion, compiledVersion string) (releaseValidationResult, error) {
	result := releaseValidationResult{SchemaVersion: 1, Valid: false}
	if requestedVersion != compiledVersion {
		return result, fmt.Errorf("requested version %q does not match candidate executable version %q", requestedVersion, compiledVersion)
	}
	manifest, err := LoadCompatibilityManifest(manifestPath, compiledVersion)
	if err != nil {
		return result, err
	}
	if manifest.Controller.Version == "" {
		return result, errors.New("validated manifest did not contain a controller version")
	}
	result.Valid = true
	result.ControllerVersion = manifest.Controller.Version
	result.ReleaseVersion = manifest.ReleaseVersion
	result.SourceSHA = manifest.Source.SHA
	result.WorkerReference = manifest.WorkerReference()
	return result, nil
}
