package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/melodic-software/ci-runner/internal/jobindex"
)

var safeJobID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (a *Application) logs(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("host logs", flag.ContinueOnError)
	flags.SetOutput(a.errOut)
	follow := flags.Bool("follow", false, "follow controller logs")
	jobID := flags.String("job", "", "show diagnostic artifacts for one job")
	cleanup := flags.Bool("cleanup", false, "run finalized worker-artifact retention now")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || boolCount(*follow, *jobID != "", *cleanup) > 1 {
		if boolCount(*follow, *jobID != "", *cleanup) > 1 {
			fmt.Fprintln(a.errOut, "--follow, --job, and --cleanup are mutually exclusive")
		}
		return ExitUsage
	}
	if a.dependencies.Logs == nil {
		fmt.Fprintln(a.errOut, "log reader is unavailable")
		return ExitInvalidConfig
	}
	if *cleanup {
		cleaner, ok := a.dependencies.Logs.(interface{ Cleanup(context.Context) error })
		if !ok {
			fmt.Fprintln(a.errOut, "artifact cleanup is unavailable")
			return ExitInvalidConfig
		}
		if err := cleaner.Cleanup(ctx); err != nil {
			fmt.Fprintf(a.errOut, "clean up worker artifacts: %v\n", err)
			return ExitRuntime
		}
		fmt.Fprintln(a.out, "Finalized worker-artifact retention cleanup completed.")
		return ExitOK
	}
	if err := a.dependencies.Logs.Write(ctx, a.out, *follow, *jobID); err != nil {
		fmt.Fprintf(a.errOut, "read logs: %v\n", err)
		return ExitRuntime
	}
	return ExitOK
}

type FileLogs struct {
	ControllerDirectory string
	WorkerLogDirectory  string
	DiagnosticDirectory string
	Jobs                jobindex.Store
	Cleaner             LogCleaner
	PollInterval        time.Duration
}

type LogCleaner interface {
	Cleanup(context.Context) error
}

type LogCleanupFunc func(context.Context) error

func (f LogCleanupFunc) Cleanup(ctx context.Context) error { return f(ctx) }

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func (f FileLogs) Write(ctx context.Context, destination io.Writer, follow bool, jobID string) error {
	if jobID != "" {
		return f.writeJobArtifacts(ctx, destination, jobID)
	}
	interval := f.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	var lastPath string
	var offset int64
	for {
		path, err := newestRegularFile(f.ControllerDirectory)
		if err != nil {
			return err
		}
		if path != lastPath {
			lastPath = path
			offset = 0
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			_ = file.Close()
			return err
		}
		written, copyErr := io.Copy(destination, file)
		offset += written
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		if !follow {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

func (f FileLogs) Cleanup(ctx context.Context) error {
	if f.Cleaner == nil {
		return errors.New("artifact cleanup is unavailable")
	}
	return f.Cleaner.Cleanup(ctx)
}

func (f FileLogs) writeJobArtifacts(ctx context.Context, destination io.Writer, jobID string) error {
	if !safeJobID.MatchString(jobID) {
		return errors.New("job ID contains unsupported characters")
	}
	if f.Jobs == nil {
		return errors.New("job index is unavailable")
	}
	record, err := f.Jobs.FindByJobID(ctx, jobID)
	if errors.Is(err, jobindex.ErrNotFound) {
		return fmt.Errorf("no diagnostic artifact found for job %q", jobID)
	}
	if err != nil {
		return err
	}
	paths := []struct {
		root string
		path string
	}{
		{root: f.WorkerLogDirectory, path: record.LogPath},
		{root: f.DiagnosticDirectory, path: record.DiagnosticPath},
		{root: f.DiagnosticDirectory, path: record.ResourcePath},
	}
	written := 0
	for _, artifact := range paths {
		if artifact.path == "" {
			continue
		}
		if err := validateArtifactReadPath(artifact.root, artifact.path); err != nil {
			return err
		}
		info, statErr := os.Lstat(artifact.path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode().IsRegular() {
			fmt.Fprintln(destination, artifact.path)
			written++
		}
	}
	if written == 0 {
		return fmt.Errorf("no diagnostic artifact found for job %q", jobID)
	}
	return nil
}

func validateArtifactReadPath(root, path string) error {
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return errors.New("artifact root and indexed path must be absolute")
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("indexed artifact path %q escapes configured root %q", path, root)
	}
	if err := ensureNoReparsePoints(path); err != nil {
		return fmt.Errorf("indexed artifact path %q is unsafe: %w", path, err)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("indexed artifact path %q is not a regular file", path)
	}
	return nil
}

func newestRegularFile(directory string) (string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", err
	}
	type candidate struct {
		path string
		at   time.Time
	}
	var files []candidate
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return "", err
		}
		files = append(files, candidate{path: filepath.Join(directory, entry.Name()), at: info.ModTime()})
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no controller log files in %q", directory)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].at.Equal(files[j].at) {
			return files[i].path < files[j].path
		}
		return files[i].at.Before(files[j].at)
	})
	return files[len(files)-1].path, nil
}
