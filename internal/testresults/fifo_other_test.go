//go:build !unix

package testresults

import "errors"

func mkfifo(string) error { return errors.New("no FIFOs here") }
