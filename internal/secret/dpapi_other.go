//go:build !windows

package secret

import "errors"

type DPAPIProtector struct{}

func NewDPAPIProtector() DPAPIProtector { return DPAPIProtector{} }

func (DPAPIProtector) Protect([]byte, string) ([]byte, error) {
	return nil, errors.New("current-user DPAPI requires Windows")
}

func (DPAPIProtector) Unprotect([]byte) ([]byte, error) {
	return nil, errors.New("current-user DPAPI requires Windows")
}
