//go:build !linux

package indicator

import (
	"errors"
	"os"
)

type sysMounter struct{}

func (sysMounter) Bind(string, string) error { return errors.ErrUnsupported }
func (sysMounter) Unbind(string) error       { return errors.ErrUnsupported }
func (sysMounter) Real(*os.File) error       { return errors.ErrUnsupported }
