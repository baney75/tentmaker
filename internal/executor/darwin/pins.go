package darwin

// Pinned downloads and images, resolved 2026-11-21 through the GitHub
// releases API and ghcr.io (token + manifest HEAD, as in fleet-ci's "Update
// the base image"). Tart and Softnet moved from cirruslabs to openai on
// GitHub; the VM images stay on ghcr.io/cirruslabs.
const (
	ResolvedOn = "2026-11-21"

	// Tart 2.42.0 (2026-11-17), the newest stable release: the GitHub asset
	// digest of tart.tar.gz.
	TartVersion = "2.42.0"
	TartSHA256  = "a95dda214b5f676298430d665a8174992e6b300dd20c653385b60481ae819ddf"
	TartURL     = "https://github.com/openai/tart/releases/download/" + TartVersion + "/tart.tar.gz"

	// Softnet 0.26.0 (2026-11-17), the newest stable release: softnet.tar.gz.
	// It needs macOS 26 or newer on the host.
	SoftnetVersion = "0.26.0"
	SoftnetSHA256  = "8469759f9cdfaaba52a150b629877985129308461702bc074c3ad7727837db82"
	SoftnetURL     = "https://github.com/openai/softnet/releases/download/" + SoftnetVersion + "/softnet.tar.gz"

	// actions/runner 2.338.2 (2026-10-26), checked inside the guest. The
	// hashes match the release body and GitHub's asset digests.
	RunnerVersion      = "2.338.2"
	RunnerSHA256OSX    = "25a065af409ffa317ab4d5a3b614dd15fc9c40f2172bc1b35a1b1258e919a388" // actions-runner-osx-arm64
	RunnerSHA256LinuxA = "670adb2f268fe21dcbb58b3db0e0aa9d5b3bddfe2c651481336e561ff86de945" // actions-runner-linux-arm64

	// ImageMacOS is macos-tahoe-xcode tag 26.5 (Xcode 26.5). fleet-ci pinned
	// this digest on 2026-10-31; the tag still resolved to it on 2026-11-21.
	// cirruslabs publishes no signature for it, so the digest is the control.
	ImageMacOS = "ghcr.io/cirruslabs/macos-tahoe-xcode@sha256:923c98d32e40ffadb6e6815a9722124b7a57bdf7d7763a708a2b28d1970831bd"
	// ImageLinux is ubuntu-runner-arm64 tag 24.04, resolved 2026-11-21.
	ImageLinux = "ghcr.io/cirruslabs/ubuntu-runner-arm64@sha256:dbc204bb40c046150ca212e5c1a38419f0f59b19d2602467dc6c67caea81186b"
)

// Softnet rules from fleet-ci. Besides Softnet's default (globally routable
// IPv4 only), block the tailnet, link-local and every connection to the
// vmnet bridge gateway, which is this Mac. DHCP hands the gateway out as the
// VM's resolver, so only its port 53 comes back. A rule with a port beats
// one without.
const (
	SoftnetBlock = "100.64.0.0/10,169.254.0.0/16,out @host"
	SoftnetAllow = "out proto udp @host port 53,out proto tcp @host port 53"
)

// RunnerPin is the actions/runner a guest downloads.
type RunnerPin struct {
	Version, Asset, SHA256 string // Asset: "osx-arm64" or "linux-arm64"
}

// URL is the release asset.
func (p RunnerPin) URL() string {
	return "https://github.com/actions/runner/releases/download/v" + p.Version +
		"/actions-runner-" + p.Asset + "-" + p.Version + ".tar.gz"
}

func runnerPin(guestOS string) RunnerPin {
	if guestOS == "macos" {
		return RunnerPin{RunnerVersion, "osx-arm64", RunnerSHA256OSX}
	}
	return RunnerPin{RunnerVersion, "linux-arm64", RunnerSHA256LinuxA}
}
