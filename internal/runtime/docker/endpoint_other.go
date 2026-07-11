//go:build !windows

package docker

const localDockerHost = "unix:///var/run/docker.sock"
