//go:build !windows

package mcp

import (
	"os/exec"
	"syscall"
)

// detach puts the child in its own session so it outlives the MCP server.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
