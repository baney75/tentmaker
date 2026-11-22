//go:build !windows

package control

import "github.com/baney75/tentmaker/internal/config"

func writeToken(path string, data []byte) error { return config.WriteAtomic(path, data, 0o600) }
