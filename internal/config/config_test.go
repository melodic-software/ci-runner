package config

import (
	"strings"
	"testing"
	"time"
)

const validYAML = `
schemaVersion: 1
host:
  id: melo-desk-001
  runnerNamePrefix: melo-desk-001
controller:
  reconcileInterval: 5s
  shutdownPollInterval: 1s
  localProbeTimeout: 15s
  startupTimeout: 2m
release:
  compatibilityManifest: 'C:\Users\runner\AppData\Local\ci-runner\release.json'
github:
  requestTimeout: 70s
  retry:
    initial: 1s
    maximum: 1m
    multiplier: 2
    jitterRatio: 0.2
    maxAttempts: 6
  targets:
    - id: melodic-org
      url: https://github.com/melodic-software
      scope: organization
      clientId: Iv23liABCDEF1234
      installationId: 12345
      secretId: melodic-org-host
      runnerGroup: ci-local-melo-desk-001
      scaleSetName: melodic-ubuntu-24.04-x64
      labels:
        - melodic-ubuntu-24.04-x64
      warmIdle: 1
      maxCapacity: 3
      priority: 0
resources:
  maximumConcurrentWorkers: 3
  worker:
    cpus: 2
    memory: 8GiB
    memorySwap: 8GiB
    pids: 4096
  minimumAvailableMemoryPercent: 25
  cpuBlockPercent: 75
  cpuResumePercent: 60
  cpuObservationWindow: 60s
  cpuHysteresisWindow: 60s
power:
  policy: always
  stableAcWindow: 30s
drain:
  warningAfter: 20m
  idleConfirmationWindow: 2s
dockerDesktop:
  startTimeout: 2m
  stopTimeout: 2m
logs:
  docker:
    driver: local
    maxSize: 10MiB
    maxFiles: 3
  controller:
    maxFileSize: 10MiB
    retention: 336h
    totalCap: 512MiB
  diagnostics:
    maxFileSize: 100MiB
    retention: 336h
    totalCap: 2GiB
  rawDiagnosticMaxInput: 512MiB
  cleanupEvery: 24h
  workerFinalizationTimeout: 2m
paths:
  secrets: 'C:\Users\runner\AppData\Local\ci-runner\secrets'
  state: 'C:\Users\runner\AppData\Local\ci-runner\state'
  logs: 'C:\Users\runner\AppData\Local\ci-runner\logs'
  diagnostics: 'C:\Users\runner\AppData\Local\ci-runner\diagnostics'
`

func TestLoadValidConfiguration(t *testing.T) {
	t.Parallel()
	cfg, err := Load(strings.NewReader(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resources.Worker.Memory != ByteSize(8<<30) {
		t.Fatalf("memory = %d", cfg.Resources.Worker.Memory)
	}
	if cfg.Drain.WarningAfter.Duration != 20*time.Minute {
		t.Fatalf("warningAfter = %s", cfg.Drain.WarningAfter.Duration)
	}
}

func TestLoadRejectsUnknownProperty(t *testing.T) {
	t.Parallel()
	_, err := Load(strings.NewReader(strings.Replace(validYAML, "  id: melo-desk-001", "  id: melo-desk-001\n  mystery: true", 1)))
	if err == nil || !strings.Contains(err.Error(), "mystery") {
		t.Fatalf("error = %v, want unknown property", err)
	}
}

func TestLoadRejectsMultipleDocuments(t *testing.T) {
	t.Parallel()
	_, err := Load(strings.NewReader(validYAML + "\n---\nschemaVersion: 1\n"))
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsInvalidUnits(t *testing.T) {
	t.Parallel()
	for _, replacement := range []string{"memory: 8GB", "cpuObservationWindow: 60"} {
		replacement := replacement
		t.Run(replacement, func(t *testing.T) {
			t.Parallel()
			input := validYAML
			if strings.HasPrefix(replacement, "memory") {
				input = strings.Replace(input, "memory: 8GiB", replacement, 1)
			} else {
				input = strings.Replace(input, "cpuObservationWindow: 60s", replacement, 1)
			}
			if _, err := Load(strings.NewReader(input)); err == nil {
				t.Fatal("expected invalid unit to fail")
			}
		})
	}
}

func TestValidateRejectsUnsafePathsDuplicatePoolsAndThresholds(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"relative path": strings.Replace(validYAML, "'C:\\Users\\runner\\AppData\\Local\\ci-runner\\state'", "'..\\state'", 1),
		"duplicate ID": strings.Replace(validYAML, "      priority: 0", `      priority: 0
    - id: melodic-org
      url: https://github.com/melodic-software/standards
      scope: repository
      clientId: Iv23liABCDEF1234
      installationId: 12345
      secretId: melodic-org-host
      runnerGroup: ''
      scaleSetName: standards
      labels: []
      warmIdle: 0
      maxCapacity: 1
      priority: 1`, 1),
		"threshold inversion":        strings.Replace(validYAML, "cpuResumePercent: 60", "cpuResumePercent: 80", 1),
		"malformed URL":              strings.Replace(validYAML, "https://github.com/melodic-software", "https://example.com/melodic-software", 1),
		"raw diagnostics bound":      strings.Replace(validYAML, "rawDiagnosticMaxInput: 512MiB", "rawDiagnosticMaxInput: 50MiB", 1),
		"UNC path":                   strings.Replace(validYAML, `'C:\Users\runner\AppData\Local\ci-runner\state'`, `'\\server\share\state'`, 1),
		"device namespace":           strings.Replace(validYAML, `'C:\Users\runner\AppData\Local\ci-runner\state'`, `'\\?\C:\ci-runner\state'`, 1),
		"alternate data stream":      strings.Replace(validYAML, `'C:\Users\runner\AppData\Local\ci-runner\state'`, `'C:\ci-runner\state:evil'`, 1),
		"reserved device":            strings.Replace(validYAML, `'C:\Users\runner\AppData\Local\ci-runner\state'`, `'C:\ci-runner\CON\state'`, 1),
		"nested runtime roots":       strings.Replace(validYAML, `'C:\Users\runner\AppData\Local\ci-runner\diagnostics'`, `'C:\Users\runner\AppData\Local\ci-runner\state\diagnostics'`, 1),
		"canonical equivalent roots": strings.Replace(validYAML, `'C:\Users\runner\AppData\Local\ci-runner\diagnostics'`, `'c:/users/RUNNER/AppData/Local/ci-runner/state'`, 1),
	}
	for name, input := range tests {
		name, input := name, input
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Load(strings.NewReader(input)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestRepositoryTargetMayOmitRunnerGroup(t *testing.T) {
	t.Parallel()
	input := strings.Replace(validYAML, "url: https://github.com/melodic-software", "url: https://github.com/kyle-sexton/dotfiles", 1)
	input = strings.Replace(input, "scope: organization", "scope: repository", 1)
	input = strings.Replace(input, "runnerGroup: ci-local-melo-desk-001", "runnerGroup: ''", 1)
	if _, err := Load(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
}

func TestTargetURLRequiresOneCanonicalGitHubRepresentation(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"https://github.com/melodic-software/",
		"https://GitHub.com/melodic-software",
		"https://github.com/MELODIC-SOFTWARE",
		"https://github.com/%6delodic-software",
	} {
		if err := validateTargetURL("target.url", raw, ScopeOrganization); err == nil {
			t.Errorf("noncanonical URL accepted: %q", raw)
		}
	}
	if err := validateTargetURL("target.url", "https://github.com/melodic-software", ScopeOrganization); err != nil {
		t.Fatalf("canonical URL rejected: %v", err)
	}
}
