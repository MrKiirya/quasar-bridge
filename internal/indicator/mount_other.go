//go:build !linux

package indicator

import "errors"

type sysMounter struct{}

func (sysMounter) Bind(string, string) error { return errors.ErrUnsupported }
func (sysMounter) Unbind(string) error       { return errors.ErrUnsupported }
