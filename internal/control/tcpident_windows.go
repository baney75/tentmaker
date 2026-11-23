package control

// TCPIdentify names a loopback TCP caller (the metrics listener's) the way
// the control API does on Windows: by the account of the process that owns
// the client end.
func TCPIdentify(runnerUser string) Identify { return HostIdentify(runnerUser) }
