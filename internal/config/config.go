// Package config loads and validates the controller's non-secret host YAML.
package config

import (
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

const SupportedSchemaVersion = 1

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
	Logs          Logs          `yaml:"logs"`
	Paths         Paths         `yaml:"paths"`
}

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
	ID             string   `yaml:"id"`
	URL            string   `yaml:"url"`
	Scope          Scope    `yaml:"scope"`
	ClientID       string   `yaml:"clientId"`
	InstallationID int64    `yaml:"installationId"`
	SecretID       string   `yaml:"secretId"`
	RunnerGroup    string   `yaml:"runnerGroup"`
	ScaleSetName   string   `yaml:"scaleSetName"`
	Labels         []string `yaml:"labels"`
	WarmIdle       int      `yaml:"warmIdle"`
	MaxCapacity    int      `yaml:"maxCapacity"`
	Priority       int      `yaml:"priority"`
}

type Resources struct {
	MaximumConcurrentWorkers  int      `yaml:"maximumConcurrentWorkers"`
	Worker                    Worker   `yaml:"worker"`
	MinimumAvailableMemoryPct float64  `yaml:"minimumAvailableMemoryPercent"`
	CPUBlockPercent           float64  `yaml:"cpuBlockPercent"`
	CPUResumePercent          float64  `yaml:"cpuResumePercent"`
	CPUObservationWindow      Duration `yaml:"cpuObservationWindow"`
	CPUHysteresisWindow       Duration `yaml:"cpuHysteresisWindow"`
}

type Worker struct {
	CPUs       float64  `yaml:"cpus"`
	Memory     ByteSize `yaml:"memory"`
	MemorySwap ByteSize `yaml:"memorySwap"`
	PIDs       int64    `yaml:"pids"`
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
	var cfg Config
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("decode configuration: multiple YAML documents are not allowed")
		}
		return Config{}, fmt.Errorf("decode configuration trailer: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var problems []error
	add := func(err error) {
		if err != nil {
			problems = append(problems, err)
		}
	}
	if c.SchemaVersion != SupportedSchemaVersion {
		add(fmt.Errorf("schemaVersion: unsupported value %d (supported: %d)", c.SchemaVersion, SupportedSchemaVersion))
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
	}
	resources := c.Resources
	if resources.MaximumConcurrentWorkers <= 0 {
		add(errors.New("resources.maximumConcurrentWorkers: must be positive"))
	}
	if resources.Worker.CPUs <= 0 || math.IsNaN(resources.Worker.CPUs) || math.IsInf(resources.Worker.CPUs, 0) {
		add(errors.New("resources.worker.cpus: must be a finite positive number"))
	}
	if resources.Worker.Memory == 0 {
		add(errors.New("resources.worker.memory: must be positive"))
	}
	if resources.Worker.MemorySwap < resources.Worker.Memory {
		add(errors.New("resources.worker.memorySwap: Docker total memory+swap must be at least memory"))
	}
	if resources.Worker.PIDs <= 0 {
		add(errors.New("resources.worker.pids: must be positive"))
	}
	add(validatePercent("resources.minimumAvailableMemoryPercent", resources.MinimumAvailableMemoryPct, false))
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
