package daemon

import "github.com/baney75/tentmaker/internal/actionscache"

func procPeers(u uplinker) actionscache.ProcPeers {
	return actionscache.ProcPeers{Proc: u.ProcDir(), Cgroup: u.UplinkCgroup}
}
