//go:build !linux && !darwin

package engine

import "os"

func openPrivateFile(string) (*os.File, error)        { return nil, ErrInvalid }
func withInterruptionLock(string, func() error) error { return ErrInvalid }
