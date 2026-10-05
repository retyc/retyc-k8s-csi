//go:build !linux

package mount

import "errors"

var errUnsupported = errors.New("mount recovery is only supported on Linux")

// Detach is Linux-only; see recover_linux.go.
func (m *Mounter) Detach(string) error { return errUnsupported }

// Rebind is Linux-only; see recover_linux.go.
func (m *Mounter) Rebind(string, string, bool) error { return errUnsupported }
