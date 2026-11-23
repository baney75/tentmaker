package linux

import (
	"os"
	"path/filepath"
	"strings"
)

// buildCacheToken is TURBO_TOKEN for a job that uses the device's build
// cache. turbo turns its remote cache on only with a token, but the cache
// names the job by its connection (internal/actionscache), so this is a
// fixed word, never a secret.
const buildCacheToken = "tentmaker-no-secret"

// buildCacheEnv is what .env adds so Turborepo uses the device's build
// cache, and so a Bazel build can opt in. base is the checked service URL
// (cacheURL). A workflow that sets its own TURBO_API keeps it: its env wins
// over .env.
//
// Bazel gets no rc file: any rc that sets --remote_cache splits a repo's
// remote execution from the cache it defaults to, whichever order Bazel
// reads it in (review first-F2, then r2 first-F3). A build opts in with
// --remote_cache=${TENTMAKER_BAZEL_CACHE}, which is empty, so no remote
// cache, on a runner that is not a Tentmaker device.
func buildCacheEnv(base string) []string {
	return []string{
		"TENTMAKER_BAZEL_CACHE=" + strings.TrimSuffix(base, "/"),
		"TURBO_API=" + strings.TrimSuffix(base, "/"),
		"TURBO_TEAM=tentmaker",
		"TURBO_TOKEN=" + buildCacheToken,
	}
}

// gradleInit is an init script for the job's ~/.gradle/init.d: it points
// the remote build cache at the device, unless the build configures its
// own remote. It leaves caching itself to the build (org.gradle.caching or
// --build-cache), so an opt-out stays out (review first-F3). Nothing in it
// may fail a build.
func gradleInit(base string) string {
	return `// Written by Tentmaker: this device's build cache for the job's repo.
// It names the job by its connection, so there is no credential here.
try {
    gradle.settingsEvaluated { settings ->
        try {
            if (settings.buildCache.remote == null) {
                settings.buildCache.remote(HttpBuildCache) { c ->
                    c.url = '` + base + `cache/'
                    if (c.hasProperty('allowInsecureProtocol')) {
                        c.allowInsecureProtocol = true
                    }
                    c.push = true
                }
            }
        } catch (Throwable t) {
            logger.info('Tentmaker build cache not set: ' + t)
        }
    }
} catch (Throwable t) {
    logger.info('Tentmaker build cache not set: ' + t)
}
`
}

// writeBuildCacheFiles writes the Gradle init script into the job home.
// home is root-owned until prepareJob hands the job dir over.
func writeBuildCacheFiles(home, base string) error {
	initd := filepath.Join(home, ".gradle", "init.d")
	if err := os.MkdirAll(initd, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(initd, "tentmaker-build-cache.gradle"), []byte(gradleInit(base)), 0o600)
}
