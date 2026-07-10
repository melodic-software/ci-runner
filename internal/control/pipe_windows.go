//go:build windows

package control

import (
	"context"
	"fmt"
	"net"
	"os/user"

	"github.com/Microsoft/go-winio"
)

func CurrentUserPipe() (string, string, error) {
	current, err := user.Current()
	if err != nil {
		return "", "", fmt.Errorf("resolve current Windows identity: %w", err)
	}
	if current.Uid == "" {
		return "", "", fmt.Errorf("current Windows identity has no SID")
	}
	path := `\\.\pipe\ci-runner-` + current.Uid
	sddl := fmt.Sprintf("D:P(A;;GA;;;SY)(A;;GA;;;%s)", current.Uid)
	return path, sddl, nil
}

func NewCurrentUserServer(handler Handler) (*Server, error) {
	path, sddl, err := CurrentUserPipe()
	if err != nil {
		return nil, err
	}
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{
		SecurityDescriptor: sddl,
		MessageMode:        false,
		InputBufferSize:    maximumMessageSize,
		OutputBufferSize:   maximumMessageSize,
	})
	if err != nil {
		return nil, fmt.Errorf("listen on current-user control pipe: %w", err)
	}
	return NewServer(listener, handler)
}

func NewCurrentUserClient() (*Client, error) {
	path, _, err := CurrentUserPipe()
	if err != nil {
		return nil, err
	}
	return NewClient(func(ctx context.Context) (net.Conn, error) {
		return winio.DialPipeContext(ctx, path)
	})
}
