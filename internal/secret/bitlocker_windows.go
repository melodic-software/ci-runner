//go:build windows

package secret

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"github.com/melodic-software/ci-runner/internal/childprocess"
)

const (
	bitLockerProtectedExit       = 0
	bitLockerNotEncryptedExit    = 10
	bitLockerProtectionOffExit   = 11
	bitLockerQueryFailedExit     = 12
	maximumSystemDirectoryLength = 32_768
)

var (
	windowsDrive           = regexp.MustCompile(`^[A-Za-z]:$`)
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemDirectory = kernel32.NewProc("GetSystemDirectoryW")
)

type WindowsBitLockerVerifier struct{}

func NewBitLockerVerifier() WindowsBitLockerVerifier { return WindowsBitLockerVerifier{} }

func (WindowsBitLockerVerifier) VerifyProtected(ctx context.Context, path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve secret path: %w", err)
	}
	volume := filepath.VolumeName(absolute)
	if !windowsDrive.MatchString(volume) {
		return fmt.Errorf("secret path %q is not on a supported local Windows volume", absolute)
	}
	powerShell, err := windowsPowerShellPath()
	if err != nil {
		return err
	}
	out, directErr := runPowerShell(ctx, powerShell, bitLockerQueryScript(volume))
	if directErr == nil {
		return parseBitLockerStatus(out)
	}

	// Get-BitLockerVolume commonly requires an elevated token even though the
	// controller intentionally runs as the limited interactive user. Elevate
	// only a read-only boolean status query. The child receives no private key,
	// source/destination path, or user-writable output path; its small exit-code
	// contract avoids a privileged file-write confused deputy.
	if err := verifyBitLockerElevated(ctx, powerShell, volume); err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			directErr = fmt.Errorf("%w (%s)", directErr, detail)
		}
		return fmt.Errorf("query BitLocker status (approve the Administrator UAC prompt when requested): direct query: %v; elevated query: %w", directErr, err)
	}
	return nil
}

func windowsPowerShellPath() (string, error) {
	systemDirectory, err := getSystemDirectory()
	if err != nil {
		return "", err
	}
	powerShell := filepath.Join(systemDirectory, "WindowsPowerShell", "v1.0", "powershell.exe")
	if !filepath.IsAbs(powerShell) {
		return "", errors.New("resolve trusted Windows system path: Windows returned a non-absolute system directory")
	}
	info, err := os.Stat(powerShell)
	if err != nil {
		return "", fmt.Errorf("inspect system Windows PowerShell: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("resolve trusted Windows system path: system Windows PowerShell is not a regular file")
	}
	return powerShell, nil
}

func getSystemDirectory() (string, error) {
	size := uint32(syscall.MAX_PATH)
	for size <= maximumSystemDirectoryLength {
		buffer := make([]uint16, size)
		length, _, callErr := procGetSystemDirectory.Call(
			uintptr(unsafe.Pointer(&buffer[0])),
			uintptr(size),
		)
		if length == 0 {
			return "", fmt.Errorf("GetSystemDirectoryW: %w", callError(callErr))
		}
		if length < uintptr(size) {
			value := syscall.UTF16ToString(buffer[:length])
			if value == "" {
				return "", errors.New("resolve trusted Windows system path: Windows returned an empty system directory")
			}
			return value, nil
		}
		size = uint32(length) + 1
	}
	return "", errors.New("resolve trusted Windows system path: Windows system directory exceeds the safety limit")
}

func bitLockerQueryScript(volume string) string {
	return fmt.Sprintf("%s$v=Get-BitLockerVolume -MountPoint '%s';[pscustomobject]@{VolumeStatus=[string]$v.VolumeStatus;ProtectionStatus=[string]$v.ProtectionStatus}|ConvertTo-Json -Compress", machineReadablePowerShellPrefix(), volume)
}

func machineReadablePowerShellPrefix() string {
	return "$ErrorActionPreference='Stop';$ProgressPreference='SilentlyContinue';$WarningPreference='SilentlyContinue';$InformationPreference='SilentlyContinue';$VerbosePreference='SilentlyContinue';$DebugPreference='SilentlyContinue';"
}

func runPowerShell(ctx context.Context, executable, script string) ([]byte, error) {
	cmd := childprocess.CommandContext(ctx, executable, powerShellArguments(script)...)
	return cmd.CombinedOutput()
}

func powerShellArguments(script string) []string {
	return []string{
		"-NoLogo", "-NoProfile", "-NonInteractive", "-OutputFormat", "Text",
		"-EncodedCommand", encodePowerShell(script),
	}
}

func verifyBitLockerElevated(ctx context.Context, powerShell, volume string) error {
	childScript := elevatedBitLockerScript(volume)
	arguments := strings.Join(powerShellArguments(childScript), " ")
	outerScript := elevatedBitLockerLauncherScript(powerShell, arguments)
	out, err := runPowerShell(ctx, powerShell, outerScript)
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = "the UAC prompt may have been declined"
		}
		return fmt.Errorf("start elevated Get-BitLockerVolume: %w (%s)", err, detail)
	}
	return parseElevatedBitLockerExit(out)
}

func elevatedBitLockerLauncherScript(powerShell, arguments string) string {
	return fmt.Sprintf(
		"%s$p=Start-Process -FilePath '%s' -Verb RunAs -WindowStyle Hidden -ArgumentList '%s' -Wait -PassThru;[Console]::Out.Write([int]$p.ExitCode)",
		machineReadablePowerShellPrefix(),
		quotePowerShellLiteral(powerShell),
		quotePowerShellLiteral(arguments),
	)
}

func parseElevatedBitLockerExit(out []byte) error {
	exitCode, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return fmt.Errorf("parse elevated BitLocker status exit code %q: %w", strings.TrimSpace(string(out)), err)
	}
	switch exitCode {
	case bitLockerProtectedExit:
		return nil
	case bitLockerNotEncryptedExit:
		return fmt.Errorf("BitLocker volume status is not FullyEncrypted")
	case bitLockerProtectionOffExit:
		return fmt.Errorf("BitLocker protection status is not On")
	case bitLockerQueryFailedExit:
		return fmt.Errorf("elevated Get-BitLockerVolume failed")
	default:
		return fmt.Errorf("elevated BitLocker verifier returned unsupported exit code %d", exitCode)
	}
}

func elevatedBitLockerScript(volume string) string {
	return fmt.Sprintf(`%stry{$v=Get-BitLockerVolume -MountPoint '%s';if([string]$v.VolumeStatus -ne 'FullyEncrypted'){exit %d};if([string]$v.ProtectionStatus -ne 'On'){exit %d};exit %d}catch{exit %d}`,
		machineReadablePowerShellPrefix(), quotePowerShellLiteral(volume), bitLockerNotEncryptedExit, bitLockerProtectionOffExit,
		bitLockerProtectedExit, bitLockerQueryFailedExit)
}

func quotePowerShellLiteral(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func encodePowerShell(script string) string {
	encoded := utf16.Encode([]rune(script))
	bytes := make([]byte, len(encoded)*2)
	for index, value := range encoded {
		bytes[index*2] = byte(value)
		bytes[index*2+1] = byte(value >> 8)
	}
	return base64.StdEncoding.EncodeToString(bytes)
}
