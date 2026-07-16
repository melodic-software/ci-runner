// Package config loads and validates the controller's non-secret host YAML.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	LegacySchemaVersion    = 1
	SupportedSchemaVersion = 2
)

var (
	hostIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	prefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	sizePattern   = regexp.MustCompile(`^([1-9][0-9]*)(B|KiB|MiB|GiB)$`)
	windowsAbs    = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	windowsDevice = regexp.MustCompile(`(?i)^(CON|PRN|AUX|NUL|COM[1-9]|LPT[1-9])(?:\..*)?$`)
	githubSegment = regexp.MustCompile(`^[a-z0-9_.-]+$`)
)

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return errors.New("duration must be a quoted or plain string with an explicit unit")
	}
	v, err := time.ParseDuration(node.Value)
	if err != nil || v <= 0 {
		return fmt.Errorf("invalid positive duration %q", node.Value)
	}
	d.Duration = v
	return nil
}

type ByteSize uint64

func (s *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return errors.New("size must be a string with one of B, KiB, MiB, or GiB")
	}
	m := sizePattern.FindStringSubmatch(node.Value)
	if m == nil {
		return fmt.Errorf("invalid byte size %q (use B, KiB, MiB, or GiB)", node.Value)
	}
	n, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid byte size %q: %w", node.Value, err)
	}
	multiplier := uint64(1)
	switch m[2] {
	case "KiB":
		multiplier = 1 << 10
	case "MiB":
		multiplier = 1 << 20
	case "GiB":
		multiplier = 1 << 30
	}
	if n > math.MaxUint64/multiplier {
		return fmt.Errorf("byte size %q overflows uint64", node.Value)
	}
	*s = ByteSize(n * multiplier)
	return nil
}

type Config struct {
	SchemaVersion int           `yaml:"schemaVersion"`
	Host          Host          `yaml:"host"`
	Controller    Controller    `yaml:"controller"`
	Release       Release       `yaml:"release"`
	GitHub        GitHub        `yaml:"github"`
	Resources     Resources     `yaml:"resources"`
	Power         Power         `yaml:"power"`
	Drain         Drain         `yaml:"drain"`
	DockerDesktop DockerDesktop `yaml:"dockerDesktop"`
	WorkerImage   WorkerImage   `yaml:"workerImage"`
	Logs          Logs          `yaml:"logs"`
	Telemetry     Telemetry     `yaml:"telemetry"`
	Paths         Paths         `yaml:"paths"`
}

type Telemetry struct {
	Endpoint             string   `yaml:"endpoint"`
	Protocol             string   `yaml:"protocol"`
	Traces               bool     `yaml:"traces"`
	Metrics              bool     `yaml:"metrics"`
	MetricExportInterval Duration `yaml:"metricExportInterval"`
	MetricExportTimeout  Duration `yaml:"metricExportTimeout"`
}

func (t Telemetry) Enabled() bool { return t.Endpoint != "" }

type Host struct {
	ID               string `yaml:"id"`
	RunnerNamePrefix string `yaml:"runnerNamePrefix"`
}

type Controller struct {
	ReconcileInterval    Duration `yaml:"reconcileInterval"`
	ShutdownPollInterval Duration `yaml:"shutdownPollInterval"`
	LocalProbeTimeout    Duration `yaml:"localProbeTimeout"`
	StartupTimeout       Duration `yaml:"startupTimeout"`
}

type Release struct {
	CompatibilityManifest string `yaml:"compatibilityManifest"`
}

type GitHub struct {
	RequestTimeout Duration `yaml:"requestTimeout"`
	Retry          Retry    `yaml:"retry"`
	Targets        []Target `yaml:"targets"`
}

type Retry struct {
	Initial     Duration `yaml:"initial"`
	Maximum     Duration `yaml:"maximum"`
	Multiplier  float64  `yaml:"multiplier"`
	JitterRatio float64  `yaml:"jitterRatio"`
	MaxAttempts int      `yaml:"maxAttempts"`
}

type Scope string

const (
	ScopeOrganization Scope = "organization"
	ScopeRepository   Scope = "repository"
)

type Target struct {
	ID             string          `yaml:"id"`
	URL            string          `yaml:"url"`
	Scope          Scope           `yaml:"scope"`
	ClientID       string          `yaml:"clientId"`
	InstallationID int64           `yaml:"installationId"`
	SecretID       string          `yaml:"secretId"`
	RunnerGroup    string          `yaml:"runnerGroup"`
	ScaleSetName   string          `yaml:"scaleSetName"`
	Labels         []string        `yaml:"labels"`
	WarmIdle       int             `yaml:"warmIdle"`
	MaxCapacity    int             `yaml:"maxCapacity"`
	Priority       int             `yaml:"priority"`
	Resources      TargetResources `yaml:"resources"`
}

type TargetResources struct {
	Worker *WorkerOverrides `yaml:"worker"`
}

// WorkerOverrides overlays only explicitly configured fields on the global
// worker profile. Pointers preserve omitted fields while explicit scalar values
// (including invalid zeroes) reach validation; Load rejects null and blank nodes
// before they can be mistaken for omission.
type WorkerOverrides struct {
	CPUs       *float64  `yaml:"cpus"`
	Memory     *ByteSize `yaml:"memory"`
	MemorySwap *ByteSize `yaml:"memorySwap"`
	PIDs       *int64    `yaml:"pids"`
}

type Resources struct {
	MaximumConcurrentWorkers        int      `yaml:"maximumConcurrentWorkers"`
	Worker                          Worker   `yaml:"worker"`
	MinimumAvailableMemoryPct       float64  `yaml:"minimumAvailableMemoryPercent"`
	MemoryCapacityIncreaseMarginPct float64  `yaml:"memoryCapacityIncreaseMarginPercent"`
	CPUBlockPercent                 float64  `yaml:"cpuBlockPercent"`
	CPUResumePercent                float64  `yaml:"cpuResumePercent"`
	CPUObservationWindow            Duration `yaml:"cpuObservationWindow"`
	CPUHysteresisWindow             Duration `yaml:"cpuHysteresisWindow"`
}

type Worker struct {
	CPUs       float64  `yaml:"cpus"`
	Memory     ByteSize `yaml:"memory"`
	MemorySwap ByteSize `yaml:"memorySwap"`
	PIDs       int64    `yaml:"pids"`
}

// EffectiveWorker returns the target worker profile after applying optional
// field-level overrides to the global defaults.
func (t Target) EffectiveWorker(defaults Worker) Worker {
	if t.Resources.Worker == nil {
		return defaults
	}
	overrides := *t.Resources.Worker
	if overrides.CPUs != nil {
		defaults.CPUs = *overrides.CPUs
	}
	if overrides.Memory != nil {
		defaults.Memory = *overrides.Memory
	}
	if overrides.MemorySwap != nil {
		defaults.MemorySwap = *overrides.MemorySwap
	}
	if overrides.PIDs != nil {
		defaults.PIDs = *overrides.PIDs
	}
	return defaults
}

// WorkerForTarget resolves the exact runtime profile for a configured target.
func (c Config) WorkerForTarget(id string) (Worker, bool) {
	for _, target := range c.GitHub.Targets {
		if target.ID == id {
			return target.EffectiveWorker(c.Resources.Worker), true
		}
	}
	return Worker{}, false
}

type PowerPolicy string

const (
	PowerAlways PowerPolicy = "always"
	PowerACOnly PowerPolicy = "ac-only"
)

type Power struct {
	Policy         PowerPolicy `yaml:"policy"`
	StableACWindow Duration    `yaml:"stableAcWindow"`
}

type Drain struct {
	WarningAfter           Duration `yaml:"warningAfter"`
	IdleConfirmationWindow Duration `yaml:"idleConfirmationWindow"`
}

type DockerDesktop struct {
	StartTimeout Duration `yaml:"startTimeout"`
	StopTimeout  Duration `yaml:"stopTimeout"`
}

// WorkerImage configures the disposable worker's own pinned Docker image, as
// opposed to DockerDesktop's host application lifecycle. It is deliberately
// its own struct rather than a field on Resources.Worker: that type also
// backs controller.StartWorkerRequest.Limits (the per-worker container
// resource limits passed into every Start call), so adding an unrelated
// pull-timing concern there would leak into a struct callers reuse for a
// different purpose. There is exactly one worker image per host (unlike
// Resources.Worker, it is never overridden per target), so it gets a
// dedicated, non-overridable home instead.
//
// PullTimeout is a deliberate, acknowledged exception to every other Duration
// field in this file (DockerDesktop.StartTimeout/StopTimeout,
// Drain.WarningAfter/IdleConfirmationWindow, Power.StableACWindow, and the
// rest): those are all hard-required-positive with no code-level default,
// because each encodes a real operator policy choice this schema deliberately
// forces an explicit YAML value for. PullTimeout is not that kind of field --
// it is a generic infrastructure timeout with no meaningful behavior/policy
// tradeoff attached (unlike, say, a memory-margin knob), so requiring
// explicit opt-in buys nothing and only risks breaking every already-deployed
// host config.yaml on an ordinary controller upgrade the moment this field is
// introduced. Load defaults it to defaultWorkerImagePullTimeout when omitted
// (left at its YAML zero value); see Validate's use of "< 0" rather than
// "<= 0" for this field specifically, reflecting that only a genuinely
// negative explicit value is ever rejected.
type WorkerImage struct {
	PullTimeout Duration `yaml:"pullTimeout"`
}

// defaultWorkerImagePullTimeout is applied by Load when workerImage.pullTimeout
// is omitted from the host YAML (see WorkerImage's doc comment for why this
// field alone gets a code-level default). 20 minutes matches this codebase's
// own prior considered judgment (reconcileStepWorkerImagePullBudget's history
// in internal/app/controller_main.go) for a generous, safe bound on a
// first-run or updated-digest pull of a multi-gigabyte CI runner image over a
// slow link.
const defaultWorkerImagePullTimeout = 20 * time.Minute

type Logs struct {
	Docker                    DockerLogs `yaml:"docker"`
	Controller                LogClass   `yaml:"controller"`
	Diagnostics               LogClass   `yaml:"diagnostics"`
	RawDiagnosticMaxInput     ByteSize   `yaml:"rawDiagnosticMaxInput"`
	CleanupEvery              Duration   `yaml:"cleanupEvery"`
	WorkerFinalizationTimeout Duration   `yaml:"workerFinalizationTimeout"`
}

type DockerLogs struct {
	Driver   string   `yaml:"driver"`
	MaxSize  ByteSize `yaml:"maxSize"`
	MaxFiles int      `yaml:"maxFiles"`
}

type LogClass struct {
	MaxFileSize ByteSize `yaml:"maxFileSize"`
	Retention   Duration `yaml:"retention"`
	TotalCap    ByteSize `yaml:"totalCap"`
}

type Paths struct {
	Secrets     string `yaml:"secrets"`
	State       string `yaml:"state"`
	Logs        string `yaml:"logs"`
	Diagnostics string `yaml:"diagnostics"`
}

// Load reads exactly one YAML document, rejects unknown fields, and validates
// every field before returning it to policy code.
func Load(r io.Reader) (Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Config{}, fmt.Errorf("read configuration: %w", err)
	}
	var document yaml.Node
	syntax := yaml.NewDecoder(bytes.NewReader(data))
	if err := syntax.Decode(&document); err != nil {
		return Config{}, fmt.Errorf("decode configuration syntax tree: %w", err)
	}
	var extra any
	if err := syntax.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("decode configuration: multiple YAML documents are not allowed")
		}
		return Config{}, fmt.Errorf("decode configuration trailer: %w", err)
	}
	if err := rejectYAMLMergeKeys(&document); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	if err := validateTargetResourceSyntax(&document); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	if err := validateResourceSchemaSyntax(&document); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	// See WorkerImage's doc comment for why this is the one Duration field in
	// this schema that gets a code-level default instead of being required.
	if cfg.WorkerImage.PullTimeout.Duration == 0 {
		cfg.WorkerImage.PullTimeout.Duration = defaultWorkerImagePullTimeout
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateResourceSchemaSyntax(document *yaml.Node) error {
	root := dereferenceYAMLNode(document)
	if root == nil || root.Kind != yaml.DocumentNode || len(root.Content) != 1 {
		return nil
	}
	root = dereferenceYAMLNode(root.Content[0])
	schema, ok := yamlMappingValue(root, "schemaVersion")
	if !ok {
		return nil
	}
	schema = dereferenceYAMLNode(schema)
	if schema == nil {
		return nil
	}
	schemaVersion, err := strconv.Atoi(schema.Value)
	if err != nil {
		return nil
	}
	resources, ok := yamlMappingValue(root, "resources")
	if !ok {
		return nil
	}
	resources = dereferenceYAMLNode(resources)
	if resources == nil {
		return nil
	}
	margin, present := yamlMappingValue(resources, "memoryCapacityIncreaseMarginPercent")
	switch schemaVersion {
	case LegacySchemaVersion:
		if present {
			return errors.New("resources.memoryCapacityIncreaseMarginPercent is not defined by schemaVersion 1")
		}
	case SupportedSchemaVersion:
		if !present || yamlNodeIsNull(dereferenceYAMLNode(margin)) {
			return errors.New("resources.memoryCapacityIncreaseMarginPercent is required by schemaVersion 2")
		}
	}
	return nil
}

func rejectYAMLMergeKeys(document *yaml.Node) error {
	visited := make(map[*yaml.Node]struct{})
	var walk func(*yaml.Node) error
	walk = func(node *yaml.Node) error {
		if node == nil {
			return nil
		}
		if _, seen := visited[node]; seen {
			return nil
		}
		visited[node] = struct{}{}
		if node.Kind == yaml.AliasNode {
			return walk(node.Alias)
		}
		if node.Kind == yaml.MappingNode {
			for index := 0; index+1 < len(node.Content); index += 2 {
				key := node.Content[index]
				if key.Tag == "!!merge" || key.Value == "<<" {
					return errors.New("YAML merge keys (<<) are not allowed")
				}
			}
		}
		for _, child := range node.Content {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(document)
}

func validateTargetResourceSyntax(document *yaml.Node) error {
	root := dereferenceYAMLNode(document)
	if root == nil || root.Kind != yaml.DocumentNode || len(root.Content) != 1 {
		return nil
	}
	root = dereferenceYAMLNode(root.Content[0])
	github, ok := yamlMappingValue(root, "github")
	if !ok {
		return nil
	}
	targets, ok := yamlMappingValue(dereferenceYAMLNode(github), "targets")
	if !ok {
		return nil
	}
	targets = dereferenceYAMLNode(targets)
	if targets == nil || targets.Kind != yaml.SequenceNode {
		return nil
	}
	for index, rawTarget := range targets.Content {
		target := dereferenceYAMLNode(rawTarget)
		resources, present := yamlMappingValue(target, "resources")
		if !present {
			continue
		}
		path := fmt.Sprintf("github.targets[%d].resources", index)
		resources = dereferenceYAMLNode(resources)
		if yamlNodeIsNull(resources) {
			return fmt.Errorf("%s: must not be null or blank", path)
		}
		worker, present := yamlMappingValue(resources, "worker")
		if !present {
			continue
		}
		path += ".worker"
		worker = dereferenceYAMLNode(worker)
		if yamlNodeIsNull(worker) {
			return fmt.Errorf("%s: must not be null or blank", path)
		}
		for _, field := range []string{"cpus", "memory", "memorySwap", "pids"} {
			value, fieldPresent := yamlMappingValue(worker, field)
			if fieldPresent && yamlNodeIsNull(dereferenceYAMLNode(value)) {
				return fmt.Errorf("%s.%s: must not be null or blank", path, field)
			}
		}
	}
	return nil
}

func yamlMappingValue(mapping *yaml.Node, key string) (*yaml.Node, bool) {
	mapping = dereferenceYAMLNode(mapping)
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, false
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1], true
		}
	}
	return nil, false
}

func dereferenceYAMLNode(node *yaml.Node) *yaml.Node {
	for node != nil && node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	return node
}

func yamlNodeIsNull(node *yaml.Node) bool {
	return node != nil && node.Tag == "!!null"
}

func (c Config) Validate() error {
	var problems []error
	add := func(err error) {
		if err != nil {
			problems = append(problems, err)
		}
	}
	if c.SchemaVersion != LegacySchemaVersion && c.SchemaVersion != SupportedSchemaVersion {
		add(fmt.Errorf("schemaVersion: unsupported value %d (supported: %d, %d)", c.SchemaVersion, LegacySchemaVersion, SupportedSchemaVersion))
	}
	if !hostIDPattern.MatchString(c.Host.ID) {
		add(errors.New("host.id: must be a lowercase DNS-style machine identifier"))
	}
	if !prefixPattern.MatchString(c.Host.RunnerNamePrefix) {
		add(errors.New("host.runnerNamePrefix: must contain only letters, digits, dot, underscore, or hyphen and be at most 64 characters"))
	}
	if c.Controller.ReconcileInterval.Duration <= 0 || c.Controller.ShutdownPollInterval.Duration <= 0 || c.Controller.LocalProbeTimeout.Duration <= 0 || c.Controller.StartupTimeout.Duration <= 0 {
		add(errors.New("controller: reconcileInterval, shutdownPollInterval, localProbeTimeout, and startupTimeout must be positive"))
	}
	add(validateSafeAbsolutePath("release.compatibilityManifest", c.Release.CompatibilityManifest, false))
	if len(c.GitHub.Targets) == 0 {
		add(errors.New("github.targets: at least one target is required"))
	}
	if c.GitHub.RequestTimeout.Duration <= 0 {
		add(errors.New("github.requestTimeout: must be positive"))
	}
	if c.GitHub.Retry.Initial.Duration <= 0 || c.GitHub.Retry.Maximum.Duration < c.GitHub.Retry.Initial.Duration {
		add(errors.New("github.retry: maximum must be at least a positive initial duration"))
	}
	if c.GitHub.Retry.Multiplier < 1 || math.IsNaN(c.GitHub.Retry.Multiplier) || math.IsInf(c.GitHub.Retry.Multiplier, 0) {
		add(errors.New("github.retry.multiplier: must be a finite number at least 1"))
	}
	if c.GitHub.Retry.JitterRatio < 0 || c.GitHub.Retry.JitterRatio > 1 || math.IsNaN(c.GitHub.Retry.JitterRatio) {
		add(errors.New("github.retry.jitterRatio: must be between 0 and 1"))
	}
	if c.GitHub.Retry.MaxAttempts <= 0 {
		add(errors.New("github.retry.maxAttempts: must be positive"))
	}
	seenIDs := map[string]struct{}{}
	seenScaleSets := map[string]struct{}{}
	for i, target := range c.GitHub.Targets {
		path := fmt.Sprintf("github.targets[%d]", i)
		if !prefixPattern.MatchString(target.ID) {
			add(fmt.Errorf("%s.id: invalid stable target ID", path))
		}
		if _, ok := seenIDs[target.ID]; ok {
			add(fmt.Errorf("%s.id: duplicate target ID %q", path, target.ID))
		}
		seenIDs[target.ID] = struct{}{}
		add(validateTargetURL(path+".url", target.URL, target.Scope))
		if !regexp.MustCompile(`^[A-Za-z0-9_-]{6,128}$`).MatchString(target.ClientID) {
			add(fmt.Errorf("%s.clientId: must be a GitHub App client ID", path))
		}
		if target.InstallationID <= 0 {
			add(fmt.Errorf("%s.installationId: must be positive", path))
		}
		if !prefixPattern.MatchString(target.SecretID) {
			add(fmt.Errorf("%s.secretId: must identify a DPAPI-protected host credential", path))
		}
		if target.Scope == ScopeOrganization && strings.TrimSpace(target.RunnerGroup) == "" {
			add(fmt.Errorf("%s.runnerGroup: required for organization targets", path))
		}
		if target.Scope == ScopeRepository && strings.TrimSpace(target.RunnerGroup) != "" {
			add(fmt.Errorf("%s.runnerGroup: repository targets must not name an organization runner group", path))
		}
		if !prefixPattern.MatchString(target.ScaleSetName) {
			add(fmt.Errorf("%s.scaleSetName: invalid scale-set name", path))
		}
		seenLabels := map[string]struct{}{}
		for labelIndex, label := range target.Labels {
			if !prefixPattern.MatchString(label) {
				add(fmt.Errorf("%s.labels[%d]: invalid runner label", path, labelIndex))
			}
			if _, exists := seenLabels[label]; exists {
				add(fmt.Errorf("%s.labels[%d]: duplicate runner label %q", path, labelIndex, label))
			}
			seenLabels[label] = struct{}{}
		}
		key := target.URL + "\x00" + target.RunnerGroup + "\x00" + target.ScaleSetName
		if _, ok := seenScaleSets[key]; ok {
			add(fmt.Errorf("%s: duplicate URL, runner group, and scale-set name", path))
		}
		seenScaleSets[key] = struct{}{}
		if target.WarmIdle < 0 {
			add(fmt.Errorf("%s.warmIdle: must not be negative", path))
		}
		if target.MaxCapacity <= 0 {
			add(fmt.Errorf("%s.maxCapacity: must be positive", path))
		}
		if target.WarmIdle > target.MaxCapacity {
			add(fmt.Errorf("%s: warmIdle must not exceed maxCapacity", path))
		}
		if target.Priority < 0 {
			add(fmt.Errorf("%s.priority: must not be negative", path))
		}
		if target.Resources.Worker != nil {
			add(validateWorker(path+".resources.worker", target.EffectiveWorker(c.Resources.Worker)))
		}
	}
	resources := c.Resources
	if resources.MaximumConcurrentWorkers <= 0 {
		add(errors.New("resources.maximumConcurrentWorkers: must be positive"))
	}
	add(validateWorker("resources.worker", resources.Worker))
	add(validatePercent("resources.minimumAvailableMemoryPercent", resources.MinimumAvailableMemoryPct, false))
	if c.SchemaVersion == LegacySchemaVersion {
		if resources.MemoryCapacityIncreaseMarginPct != 0 {
			add(errors.New("resources.memoryCapacityIncreaseMarginPercent: schemaVersion 1 requires the legacy zero default"))
		}
	} else {
		add(validatePercent("resources.memoryCapacityIncreaseMarginPercent", resources.MemoryCapacityIncreaseMarginPct, false))
	}
	add(validatePercent("resources.cpuBlockPercent", resources.CPUBlockPercent, false))
	add(validatePercent("resources.cpuResumePercent", resources.CPUResumePercent, true))
	if resources.CPUResumePercent >= resources.CPUBlockPercent {
		add(errors.New("resources: cpuResumePercent must be lower than cpuBlockPercent"))
	}
	if resources.CPUObservationWindow.Duration <= 0 {
		add(errors.New("resources.cpuObservationWindow: must be positive"))
	}
	if resources.CPUHysteresisWindow.Duration <= 0 {
		add(errors.New("resources.cpuHysteresisWindow: must be positive"))
	}
	if c.Power.Policy != PowerAlways && c.Power.Policy != PowerACOnly {
		add(errors.New("power.policy: must be always or ac-only"))
	}
	if c.Power.StableACWindow.Duration <= 0 {
		add(errors.New("power.stableAcWindow: must be positive"))
	}
	if c.Drain.WarningAfter.Duration <= 0 {
		add(errors.New("drain.warningAfter: must be positive"))
	}
	if c.Drain.IdleConfirmationWindow.Duration <= 0 {
		add(errors.New("drain.idleConfirmationWindow: must be positive"))
	}
	if c.DockerDesktop.StartTimeout.Duration <= 0 || c.DockerDesktop.StopTimeout.Duration <= 0 {
		add(errors.New("dockerDesktop: startTimeout and stopTimeout must be positive"))
	}
	// Unlike every other Duration field validated in this function, zero is
	// legal here: Load defaults an omitted workerImage.pullTimeout before
	// Validate ever runs (see WorkerImage's doc comment), so only a genuinely
	// negative value -- reachable by direct Go construction of a Config, not
	// through the YAML path where Duration.UnmarshalYAML already rejects a
	// non-positive explicit value -- is rejected here.
	if c.WorkerImage.PullTimeout.Duration < 0 {
		add(errors.New("workerImage.pullTimeout: must not be negative"))
	}
	if c.Logs.Docker.Driver != "local" {
		add(errors.New("logs.docker.driver: only Docker's local driver is supported"))
	}
	if c.Logs.Docker.MaxSize == 0 || c.Logs.Docker.MaxFiles <= 0 {
		add(errors.New("logs.docker: maxSize and maxFiles must be positive"))
	}
	add(validateLogClass("logs.controller", c.Logs.Controller))
	add(validateLogClass("logs.diagnostics", c.Logs.Diagnostics))
	if c.Logs.RawDiagnosticMaxInput < c.Logs.Diagnostics.MaxFileSize {
		add(errors.New("logs.rawDiagnosticMaxInput: must be at least logs.diagnostics.maxFileSize"))
	}
	if c.Logs.CleanupEvery.Duration <= 0 {
		add(errors.New("logs.cleanupEvery: must be positive"))
	}
	if c.Logs.WorkerFinalizationTimeout.Duration <= 0 {
		add(errors.New("logs.workerFinalizationTimeout: must be positive"))
	}
	add(validateTelemetry(c.Telemetry))
	paths := []struct {
		name string
		path string
	}{
		{"paths.secrets", c.Paths.Secrets},
		{"paths.state", c.Paths.State},
		{"paths.logs", c.Paths.Logs},
		{"paths.diagnostics", c.Paths.Diagnostics},
	}
	seenPaths := map[string]string{}
	for _, item := range paths {
		err := validateSafeAbsolutePath(item.name, item.path, true)
		add(err)
		if err != nil {
			continue
		}
		canonical, _ := CanonicalWindowsLocalPath(item.path)
		for priorPath, priorName := range seenPaths {
			if pathsOverlap(canonical, priorPath) {
				add(fmt.Errorf("%s: must not equal, contain, or be contained by %s", item.name, priorName))
			}
		}
		seenPaths[canonical] = item.name
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration: %w", errors.Join(problems...))
	}
	return nil
}

func validateTelemetry(value Telemetry) error {
	configured := value.Endpoint != "" || value.Protocol != "" || value.Traces || value.Metrics ||
		value.MetricExportInterval.Duration != 0 || value.MetricExportTimeout.Duration != 0
	if !configured {
		return nil
	}
	var problems []error
	if value.Endpoint == "" {
		problems = append(problems, errors.New("telemetry.endpoint: required when telemetry is configured"))
	} else {
		u, err := url.Parse(value.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			problems = append(problems, errors.New("telemetry.endpoint: must be an http or https URL without credentials, query, or fragment"))
		}
	}
	if value.Protocol != "grpc" && value.Protocol != "http/protobuf" {
		problems = append(problems, errors.New("telemetry.protocol: must be grpc or http/protobuf"))
	}
	if !value.Traces && !value.Metrics {
		problems = append(problems, errors.New("telemetry: at least one of traces or metrics must be enabled"))
	}
	if value.Metrics {
		if value.MetricExportInterval.Duration <= 0 {
			problems = append(problems, errors.New("telemetry.metricExportInterval: must be positive when metrics are enabled"))
		}
		if value.MetricExportTimeout.Duration <= 0 {
			problems = append(problems, errors.New("telemetry.metricExportTimeout: must be positive when metrics are enabled"))
		}
	}
	return errors.Join(problems...)
}

func validateWorker(name string, worker Worker) error {
	var problems []error
	if worker.CPUs <= 0 || math.IsNaN(worker.CPUs) || math.IsInf(worker.CPUs, 0) {
		problems = append(problems, fmt.Errorf("%s.cpus: must be a finite positive number", name))
	}
	if worker.Memory == 0 {
		problems = append(problems, fmt.Errorf("%s.memory: must be positive", name))
	}
	if worker.MemorySwap < worker.Memory {
		problems = append(problems, fmt.Errorf("%s.memorySwap: Docker total memory+swap must be at least memory", name))
	}
	if worker.PIDs <= 0 {
		problems = append(problems, fmt.Errorf("%s.pids: must be positive", name))
	}
	return errors.Join(problems...)
}

func validatePercent(name string, value float64, allowZero bool) error {
	minimum := 0.0
	if !allowZero {
		minimum = math.SmallestNonzeroFloat64
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < minimum || value > 100 {
		return fmt.Errorf("%s: must be between %g and 100", name, minimum)
	}
	return nil
}

func validateLogClass(name string, class LogClass) error {
	var errs []error
	if class.MaxFileSize == 0 {
		errs = append(errs, fmt.Errorf("%s.maxFileSize: must be positive", name))
	}
	if class.Retention.Duration <= 0 {
		errs = append(errs, fmt.Errorf("%s.retention: must be positive", name))
	}
	if class.TotalCap < class.MaxFileSize {
		errs = append(errs, fmt.Errorf("%s.totalCap: must be at least maxFileSize", name))
	}
	return errors.Join(errs...)
}

func validateTargetURL(name, raw string, scope Scope) error {
	if scope != ScopeOrganization && scope != ScopeRepository {
		return fmt.Errorf("%s: scope must be organization or repository", strings.TrimSuffix(name, ".url"))
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.Port() != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return fmt.Errorf("%s: must be a canonical https://github.com URL without credentials, query, fragment, or port", name)
	}
	if !strings.HasPrefix(u.Path, "/") || strings.HasSuffix(u.Path, "/") {
		return fmt.Errorf("%s: must not contain a trailing slash or relative path", name)
	}
	segments := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	want := 1
	if scope == ScopeRepository {
		want = 2
	}
	if len(segments) != want {
		return fmt.Errorf("%s: %s scope requires %d path segment(s)", name, scope, want)
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "\\/") || !githubSegment.MatchString(segment) {
			return fmt.Errorf("%s: contains an unsafe path segment", name)
		}
	}
	canonical := "https://github.com/" + strings.Join(segments, "/")
	if raw != canonical || u.String() != canonical {
		return fmt.Errorf("%s: must use the exact lowercase canonical form %q", name, canonical)
	}
	return nil
}

func validateSafeAbsolutePath(name, raw string, directory bool) error {
	canonical, err := CanonicalWindowsLocalPath(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if directory && strings.HasSuffix(canonical, ".yaml") {
		return fmt.Errorf("%s: expected a directory path", name)
	}
	return nil
}

// CanonicalWindowsLocalPath returns the same case-insensitive, separator-stable
// representation used by configuration overlap checks. Callers can compare
// independently supplied roots without reimplementing Windows path parsing.
func CanonicalWindowsLocalPath(raw string) (string, error) {
	if raw == "" || strings.ContainsRune(raw, '\x00') || strings.ContainsAny(raw, `*?"<>|`) {
		return "", errors.New("must be a nonempty safe local Windows path")
	}
	for _, character := range raw {
		if character < 0x20 {
			return "", errors.New("control characters are not allowed")
		}
	}
	if strings.HasPrefix(raw, `\\`) || strings.HasPrefix(raw, `//`) || !windowsAbs.MatchString(raw) {
		return "", errors.New("must use a local drive-letter path; UNC and device namespaces are not allowed")
	}
	normalized := strings.ReplaceAll(raw, "/", `\`)
	if strings.HasSuffix(normalized, `\`) || strings.Contains(normalized[3:], `\\`) {
		return "", errors.New("path must not contain repeated or trailing separators")
	}
	parts := strings.Split(normalized[3:], `\`)
	if len(parts) == 0 || parts[0] == "" {
		return "", errors.New("filesystem roots are not allowed")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("empty and traversal segments are not allowed")
		}
		if strings.Contains(part, ":") {
			return "", errors.New("alternate data stream syntax is not allowed")
		}
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return "", errors.New("segments ending in a dot or space are not canonical")
		}
		if windowsDevice.MatchString(part) {
			return "", fmt.Errorf("reserved Windows device segment %q is not allowed", part)
		}
	}
	drive := strings.ToLower(normalized[:2])
	return drive + `\` + strings.ToLower(strings.Join(parts, `\`)), nil
}

func pathsOverlap(left, right string) bool {
	return left == right || strings.HasPrefix(left, right+`\`) || strings.HasPrefix(right, left+`\`)
}
