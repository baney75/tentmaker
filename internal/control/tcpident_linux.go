package control

// TCPIdentify names a loopback TCP caller (the metrics listener's) by the
// uid that owns the client end, from /proc/net/tcp and tcp6, and marks
// runnerUser's uid and its subordinate uids as CI jobs.
func TCPIdentify(runnerUser string) Identify {
	return procTCPIdentify("/proc", hostRunnerIDs(runnerUser))
}
