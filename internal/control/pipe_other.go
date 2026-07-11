//go:build !windows

package control

import "errors"

var errNamedPipeRequiresWindows = errors.New("current-user named-pipe control plane requires Windows")

func CurrentUserPipe() (string, string, error)      { return "", "", errNamedPipeRequiresWindows }
func NewCurrentUserServer(Handler) (*Server, error) { return nil, errNamedPipeRequiresWindows }
func NewCurrentUserClient() (*Client, error)        { return nil, errNamedPipeRequiresWindows }
