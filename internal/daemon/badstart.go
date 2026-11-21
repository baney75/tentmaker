//go:build tentmaker_badstart

package daemon

// A test build for the upgrade scenario (test/scenarios/upgrade.sh): its
// version --json is fine, but its daemon exits right after the update guard.
func init() {
	failStart = "this test build (tags tentmaker_badstart) exits at start"
}
