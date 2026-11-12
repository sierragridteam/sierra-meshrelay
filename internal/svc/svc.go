// Package svc installs meshbridge as an OS service: systemd on Linux, the
// Service Control Manager on Windows. macOS comes with its installer.
//
// Each platform provides Supported, Install, Uninstall and SecureConfigDir.
package svc

import "errors"

// Installed says where the service runs from and how to check on it.
type Installed struct {
	Binary  string
	CheckOn []string // commands to show the user
}

// ErrNotInstalled means the meshbridge service isn't installed here: the
// bridge runs some other way (meshbridge run, by hand).
var ErrNotInstalled = errors.New("the meshbridge service isn't installed")
