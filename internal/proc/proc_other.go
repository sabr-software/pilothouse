//go:build !darwin && !linux

package proc

import "github.com/sabr-software/pilothouse/internal/adapters"

func info(int) (adapters.Proc, error) { return adapters.Proc{}, ErrUnsupported }

func peerPID(int) (int, error) { return 0, ErrUnsupported }
