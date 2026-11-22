//go:build !linux && !darwin && !windows

package cli

import (
	"errors"
	"os"
)

func isTerminal(f *os.File) bool { return false }

func readSecret(f *os.File) ([]byte, error) {
	return nil, errors.New("no terminal support on this OS; use --token-stdin")
}

func makeRaw(f *os.File) (func(), error) {
	return nil, errors.New("no terminal support on this OS")
}
