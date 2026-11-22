//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

func isTerminal(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}

// readSecret reads a line from the console with echo off.
func readSecret(f *os.File) ([]byte, error) {
	h := windows.Handle(f.Fd())
	var old uint32
	if err := windows.GetConsoleMode(h, &old); err != nil {
		return nil, err
	}
	quiet := old&^windows.ENABLE_ECHO_INPUT | windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_LINE_INPUT
	if err := windows.SetConsoleMode(h, quiet); err != nil {
		return nil, err
	}
	defer windows.SetConsoleMode(h, old)
	return readLine(f)
}

// makeRaw puts the console on f in raw mode with virtual terminal input,
// and returns how to put it back.
func makeRaw(f *os.File) (func(), error) {
	h := windows.Handle(f.Fd())
	var old uint32
	if err := windows.GetConsoleMode(h, &old); err != nil {
		return nil, err
	}
	raw := old&^(windows.ENABLE_ECHO_INPUT|windows.ENABLE_PROCESSED_INPUT|windows.ENABLE_LINE_INPUT) | windows.ENABLE_VIRTUAL_TERMINAL_INPUT
	if err := windows.SetConsoleMode(h, raw); err != nil {
		return nil, err
	}
	return func() { _ = windows.SetConsoleMode(h, old) }, nil
}
