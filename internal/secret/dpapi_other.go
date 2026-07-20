//go:build !windows

package secret

import "errors"

var errDPAPIRequiresWindows = errors.New("current-user DPAPI requires Windows")

type DPAPIProtector struct{}

func NewDPAPIProtector() DPAPIProtector { return DPAPIProtector{} }

func (DPAPIProtector) Protect([]byte, string) ([]byte, error) {
	return nil, errDPAPIRequiresWindows
}

func (DPAPIProtector) Unprotect([]byte) ([]byte, error) {
	return nil, errDPAPIRequiresWindows
}
