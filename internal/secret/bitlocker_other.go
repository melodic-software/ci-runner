//go:build !windows

package secret

import (
	"context"
	"errors"
)

var errBitLockerRequiresWindows = errors.New("verify BitLocker protection: Windows is required")

type UnsupportedBitLockerVerifier struct{}

func NewBitLockerVerifier() UnsupportedBitLockerVerifier { return UnsupportedBitLockerVerifier{} }

func (UnsupportedBitLockerVerifier) VerifyProtected(context.Context, string) error {
	return errBitLockerRequiresWindows
}
