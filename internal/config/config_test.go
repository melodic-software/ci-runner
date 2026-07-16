package config

import (
	"strings"
	"testing"
	"time"
)

const validYAML = `
schemaVersion: 2
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
  memoryCapacityIncreaseMarginPercent: 25
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
workerImage:
  pullTimeout: 20m
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
	if cfg.Resources.MemoryCapacityIncreaseMarginPct != 25 {
		t.Fatalf("memory capacity increase margin = %g", cfg.Resources.MemoryCapacityIncreaseMarginPct)
	}
	if cfg.Drain.WarningAfter.Duration != 20*time.Minute {
		t.Fatalf("warningAfter = %s", cfg.Drain.WarningAfter.Duration)
	}
	worker, ok := cfg.WorkerForTarget("melodic-org")
	if !ok || worker != cfg.Resources.Worker {
		t.Fatalf("default effective worker = %#v, found=%v", worker, ok)
	}
	if cfg.WorkerImage.PullTimeout.Duration != 20*time.Minute {
		t.Fatalf("workerImage.pullTimeout (explicit) = %s, want 20m", cfg.WorkerImage.PullTimeout.Duration)
	}
}

// TestLoadDefaultsOmittedWorkerImagePullTimeout proves WorkerImage.PullTimeout
// is the one deliberate exception to this schema's otherwise-universal
// "every Duration is explicit/required" convention (see WorkerImage's doc
// comment): a host YAML that omits workerImage entirely must still load
// successfully, with PullTimeout defaulted to defaultWorkerImagePullTimeout,
// rather than failing Validate the way an omitted dockerDesktop.startTimeout
// or drain.warningAfter would.
func TestLoadDefaultsOmittedWorkerImagePullTimeout(t *testing.T) {
	t.Parallel()
	input := strings.Replace(validYAML, "workerImage:\n  pullTimeout: 20m\n", "", 1)
	if input == validYAML {
		t.Fatal("test fixture did not contain the expected workerImage block")
	}
	cfg, err := Load(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerImage.PullTimeout.Duration != defaultWorkerImagePullTimeout {
		t.Fatalf("workerImage.pullTimeout (omitted) = %s, want the default %s", cfg.WorkerImage.PullTimeout.Duration, defaultWorkerImagePullTimeout)
	}
}

// TestValidateRejectsNegativeWorkerImagePullTimeout proves an explicitly
// negative PullTimeout is still rejected. This is unreachable through the
// ordinary Load path -- Duration.UnmarshalYAML already refuses any
// non-positive explicit YAML value before Validate ever runs -- so, mirroring
// TestTargetWorkerOverridesUseGlobalValidationContract's load-then-mutate-
// then-Validate pattern below, this constructs the invalid value directly in
// Go and calls Validate itself, the only way a negative value can reach it
// (e.g. a future non-YAML config source).
func TestValidateRejectsNegativeWorkerImagePullTimeout(t *testing.T) {
	t.Parallel()
	cfg, err := Load(strings.NewReader(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	cfg.WorkerImage.PullTimeout.Duration = -time.Minute
	validateErr := cfg.Validate()
	if validateErr == nil || !strings.Contains(validateErr.Error(), "workerImage.pullTimeout") {
		t.Fatalf("validate error = %v, want workerImage.pullTimeout rejection", validateErr)
	}
}

func TestLoadLegacySchemaVersionOneDefaultsMemoryCapacityIncreaseMarginToZero(t *testing.T) {
	t.Parallel()
	input := strings.Replace(validYAML, "schemaVersion: 2", "schemaVersion: 1", 1)
	input = strings.Replace(input, "  memoryCapacityIncreaseMarginPercent: 25\n", "", 1)
	cfg, err := Load(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SchemaVersion != LegacySchemaVersion || cfg.Resources.MemoryCapacityIncreaseMarginPct != 0 {
		t.Fatalf("legacy config = schema %d, margin %g", cfg.SchemaVersion, cfg.Resources.MemoryCapacityIncreaseMarginPct)
	}
}

func TestLoadConfiguredTelemetry(t *testing.T) {
	t.Parallel()
	input := strings.Replace(validYAML, "paths:\n", `telemetry:
  endpoint: http://127.0.0.1:19889
  protocol: grpc
  traces: true
  metrics: true
  metricExportInterval: 15s
  metricExportTimeout: 10s
paths:
`, 1)
	cfg, err := Load(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Telemetry.Enabled() || cfg.Telemetry.Protocol != "grpc" || cfg.Telemetry.MetricExportInterval.Duration != 15*time.Second {
		t.Fatalf("telemetry = %#v", cfg.Telemetry)
	}
}

func TestValidateRejectsInvalidTelemetry(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"missing endpoint": `telemetry:
  protocol: grpc
  traces: true
`,
		"invalid endpoint": `telemetry:
  endpoint: file:///tmp/collector
  protocol: grpc
  traces: true
`,
		"invalid protocol": `telemetry:
  endpoint: http://127.0.0.1:19889
  protocol: thrift
  traces: true
`,
		"no signals": `telemetry:
  endpoint: http://127.0.0.1:19889
  protocol: grpc
`,
		"missing metric cadence": `telemetry:
  endpoint: http://127.0.0.1:19889
  protocol: grpc
  metrics: true
`,
	}
	for name, block := range tests {
		name, block := name, block
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			input := strings.Replace(validYAML, "paths:\n", block+"paths:\n", 1)
			if _, err := Load(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "telemetry") {
				t.Fatalf("error = %v, want telemetry validation failure", err)
			}
		})
	}
}

func TestTargetWorkerOverridesInheritUnspecifiedGlobalValues(t *testing.T) {
	t.Parallel()
	input := strings.Replace(validYAML, "      warmIdle: 1", `      resources:
        worker:
          cpus: 4
          memory: 24GiB
          memorySwap: 24GiB
      warmIdle: 1`, 1)
	cfg, err := Load(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	worker, ok := cfg.WorkerForTarget("melodic-org")
	if !ok {
		t.Fatal("configured target was not resolved")
	}
	want := Worker{CPUs: 4, Memory: ByteSize(24 << 30), MemorySwap: ByteSize(24 << 30), PIDs: 4096}
	if worker != want {
		t.Fatalf("effective worker = %#v, want %#v", worker, want)
	}
}

func TestTargetWorkerOverridesUseGlobalValidationContract(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		field  string
		mutate func(*Worker)
	}{
		"cpus":        {field: "cpus", mutate: func(worker *Worker) { worker.CPUs = 0 }},
		"memory":      {field: "memory", mutate: func(worker *Worker) { worker.Memory = 0 }},
		"memory swap": {field: "memorySwap", mutate: func(worker *Worker) { worker.MemorySwap = worker.Memory - 1 }},
		"pids":        {field: "pids", mutate: func(worker *Worker) { worker.PIDs = 0 }},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			global, err := Load(strings.NewReader(validYAML))
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&global.Resources.Worker)
			globalErr := global.Validate()
			if globalErr == nil || !strings.Contains(globalErr.Error(), "resources.worker."+test.field) {
				t.Fatalf("global validation error = %v", globalErr)
			}

			target, err := Load(strings.NewReader(validYAML))
			if err != nil {
				t.Fatal(err)
			}
			invalid := target.Resources.Worker
			test.mutate(&invalid)
			target.GitHub.Targets[0].Resources.Worker = &WorkerOverrides{
				CPUs: &invalid.CPUs, Memory: &invalid.Memory, MemorySwap: &invalid.MemorySwap, PIDs: &invalid.PIDs,
			}
			targetErr := target.Validate()
			wantPath := "github.targets[0].resources.worker." + test.field
			if targetErr == nil || !strings.Contains(targetErr.Error(), wantPath) {
				t.Fatalf("target validation error = %v, want %s", targetErr, wantPath)
			}
		})
	}
}

func TestLoadRejectsUnknownProperty(t *testing.T) {
	t.Parallel()
	_, err := Load(strings.NewReader(strings.Replace(validYAML, "  id: melo-desk-001", "  id: melo-desk-001\n  mystery: true", 1)))
	if err == nil || !strings.Contains(err.Error(), "mystery") {
		t.Fatalf("error = %v, want unknown property", err)
	}
}

func TestLoadRejectsNullBlankAndUnknownTargetWorkerOverrides(t *testing.T) {
	t.Parallel()
	type testCase struct {
		name     string
		fragment string
		want     string
	}
	tests := []testCase{
		{name: "null resources", fragment: "      resources: null", want: "github.targets[0].resources"},
		{name: "blank resources", fragment: "      resources:", want: "github.targets[0].resources"},
		{name: "null worker", fragment: "      resources:\n        worker: null", want: "github.targets[0].resources.worker"},
		{name: "blank worker", fragment: "      resources:\n        worker:", want: "github.targets[0].resources.worker"},
		{name: "unknown resources key", fragment: "      resources:\n        mystery: true", want: "mystery"},
		{name: "unknown worker key", fragment: "      resources:\n        worker:\n          mystery: true", want: "mystery"},
	}
	for _, field := range []string{"cpus", "memory", "memorySwap", "pids"} {
		tests = append(tests,
			testCase{name: "null " + field, fragment: "      resources:\n        worker:\n          " + field + ": null", want: "github.targets[0].resources.worker." + field},
			testCase{name: "blank " + field, fragment: "      resources:\n        worker:\n          " + field + ":", want: "github.targets[0].resources.worker." + field},
		)
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := strings.Replace(validYAML, "      warmIdle: 1", test.fragment+"\n      warmIdle: 1", 1)
			_, err := Load(strings.NewReader(input))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want rejection containing %q", err, test.want)
			}
		})
	}
}

func TestLoadRejectsYAMLMergeKeys(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"target resources merge": strings.Replace(validYAML, "      warmIdle: 1", "      resources: {<<: {worker: null}}\n      warmIdle: 1", 1),
		"worker fields merge":    strings.Replace(validYAML, "      warmIdle: 1", "      resources:\n        worker: {<<: {memory: null}}\n      warmIdle: 1", 1),
		"merge elsewhere":        strings.Replace(validYAML, "host:\n", "host:\n  <<: {id: inherited}\n", 1),
	}
	for name, input := range tests {
		name, input := name, input
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(strings.NewReader(input))
			if err == nil || !strings.Contains(err.Error(), "YAML merge keys (<<) are not allowed") {
				t.Fatalf("error = %v, want explicit merge-key rejection", err)
			}
		})
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
		"threshold inversion":         strings.Replace(validYAML, "cpuResumePercent: 60", "cpuResumePercent: 80", 1),
		"v1 field extension":          strings.Replace(validYAML, "schemaVersion: 2", "schemaVersion: 1", 1),
		"v2 missing margin":           strings.Replace(validYAML, "  memoryCapacityIncreaseMarginPercent: 25\n", "", 1),
		"v2 null margin":              strings.Replace(validYAML, "memoryCapacityIncreaseMarginPercent: 25", "memoryCapacityIncreaseMarginPercent: null", 1),
		"zero memory increase margin": strings.Replace(validYAML, "memoryCapacityIncreaseMarginPercent: 25", "memoryCapacityIncreaseMarginPercent: 0", 1),
		"malformed URL":               strings.Replace(validYAML, "https://github.com/melodic-software", "https://example.com/melodic-software", 1),
		"raw diagnostics bound":       strings.Replace(validYAML, "rawDiagnosticMaxInput: 512MiB", "rawDiagnosticMaxInput: 50MiB", 1),
		"UNC path":                    strings.Replace(validYAML, `'C:\Users\runner\AppData\Local\ci-runner\state'`, `'\\server\share\state'`, 1),
		"device namespace":            strings.Replace(validYAML, `'C:\Users\runner\AppData\Local\ci-runner\state'`, `'\\?\C:\ci-runner\state'`, 1),
		"alternate data stream":       strings.Replace(validYAML, `'C:\Users\runner\AppData\Local\ci-runner\state'`, `'C:\ci-runner\state:evil'`, 1),
		"reserved device":             strings.Replace(validYAML, `'C:\Users\runner\AppData\Local\ci-runner\state'`, `'C:\ci-runner\CON\state'`, 1),
		"nested runtime roots":        strings.Replace(validYAML, `'C:\Users\runner\AppData\Local\ci-runner\diagnostics'`, `'C:\Users\runner\AppData\Local\ci-runner\state\diagnostics'`, 1),
		"canonical equivalent roots":  strings.Replace(validYAML, `'C:\Users\runner\AppData\Local\ci-runner\diagnostics'`, `'c:/users/RUNNER/AppData/Local/ci-runner/state'`, 1),
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
