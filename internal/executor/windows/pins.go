package windows

// actions/runner 2.338.2 (2026-10-26), resolved 2026-11-21 through the GitHub
// releases API. The hashes match the release body and GitHub's asset digests.
const (
	RunnerVersion     = "2.338.2"
	RunnerSHA256X64   = "a1f17aa987fc8158bc7521d284912267a0d01b7c2c4684b33c86783b5e8221cc" // actions-runner-win-x64
	RunnerSHA256ARM64 = "0790e3f586c2c5388ed8e293e7cef01bd0e8a2c5c68dd0a628f848b61da4b3a7" // actions-runner-win-arm64
	RunnerResolvedOn  = "2026-11-21"
)

// Pin is one actions/runner zip.
type Pin struct {
	Version string
	Arch    string // "x64" or "arm64"
	SHA256  string
}

// URL is the release asset.
func (p Pin) URL() string {
	return "https://github.com/actions/runner/releases/download/v" + p.Version +
		"/actions-runner-win-" + p.Arch + "-" + p.Version + ".zip"
}

// Dir is the template's directory name under <StateDir>\runner.
func (p Pin) Dir() string { return "actions-runner-win-" + p.Arch + "-" + p.Version }

// PinFor returns the pinned runner for a host CPU.
func PinFor(arch string) (Pin, bool) {
	switch arch {
	case "x64":
		return Pin{RunnerVersion, "x64", RunnerSHA256X64}, true
	case "arm64":
		return Pin{RunnerVersion, "arm64", RunnerSHA256ARM64}, true
	}
	return Pin{}, false
}
