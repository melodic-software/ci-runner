//go:build !windows

package secret

import "context"

type UnsupportedBitLockerVerifier struct{}

func NewBitLockerVerifier() UnsupportedBitLockerVerifier { return UnsupportedBitLockerVerifier{} }

func (UnsupportedBitLockerVerifier) VerifyProtected(context.Context, string) error {
	return errBitLockerRequiresWindows
}
