package linux

// actions/runner, pinned. Resolved 2026-11-21 from
// https://api.github.com/repos/actions/runner/releases/latest (v2.338.2,
// published 2026-10-26). The hashes match both the release body's SHA-256
// list and GitHub's asset digests.
const (
	RunnerVersion     = "2.338.2"
	RunnerSHA256X64   = "baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40" // actions-runner-linux-x64-2.338.2.tar.gz
	RunnerSHA256ARM64 = "670adb2f268fe21dcbb58b3db0e0aa9d5b3bddfe2c651481336e561ff86de945" // actions-runner-linux-arm64-2.338.2.tar.gz
	RunnerResolvedOn  = "2026-11-21"
)

// Pin is one actions/runner download.
type Pin struct {
	Version string
	Arch    string // "x64" or "arm64", GitHub's spelling
	SHA256  string
}

// URL is the release asset for p.
func (p Pin) URL() string {
	return "https://github.com/actions/runner/releases/download/v" + p.Version +
		"/actions-runner-linux-" + p.Arch + "-" + p.Version + ".tar.gz"
}

// PinFor returns the pinned runner for an arch ("x64" or "arm64").
func PinFor(arch string) (Pin, bool) {
	switch arch {
	case "x64":
		return Pin{RunnerVersion, "x64", RunnerSHA256X64}, true
	case "arm64":
		return Pin{RunnerVersion, "arm64", RunnerSHA256ARM64}, true
	}
	return Pin{}, false
}
