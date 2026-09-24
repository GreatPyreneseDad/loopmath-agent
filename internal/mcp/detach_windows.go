//go:build windows

package mcp

import "os/exec"

func detach(cmd *exec.Cmd) {}
