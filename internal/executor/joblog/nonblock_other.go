//go:build !unix

package joblog

// Windows has no FIFOs a job could plant in a folder.
const nonblock = 0
