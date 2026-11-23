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
// cache. base is the checked service URL (cacheURL). A workflow that sets
// its own TURBO_API keeps it: its env wins over .env.
func buildCacheEnv(base string) []string {
	return []string{
		"TURBO_API=" + strings.TrimSuffix(base, "/"),
		"TURBO_TEAM=tentmaker",
		"TURBO_TOKEN=" + buildCacheToken,
	}
}

// bazelRC is the job's ~/.bazelrc: Bazel reads it after the workspace's
// .bazelrc, and a --remote_cache on the command line still wins.
func bazelRC(base string) string {
	return "# Written by Tentmaker: this device's build cache for the job's repo.\n" +
		"# It names the job by its connection, so there is no credential here.\n" +
		"build --remote_cache=" + strings.TrimSuffix(base, "/") + "\n"
}

// gradleInit is an init script for the job's ~/.gradle/init.d: it turns the
// build cache on and points the remote one at the device, unless the build
// configures its own remote. Nothing in it may fail a build.
func gradleInit(base string) string {
	return `// Written by Tentmaker: this device's build cache for the job's repo.
// It names the job by its connection, so there is no credential here.
try {
    gradle.startParameter.buildCacheEnabled = true
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

// writeBuildCacheFiles writes the job home's .bazelrc and Gradle init
// script. home is root-owned until prepareJob hands the job dir over.
func writeBuildCacheFiles(home, base string) error {
	if err := os.WriteFile(filepath.Join(home, ".bazelrc"), []byte(bazelRC(base)), 0o600); err != nil {
		return err
	}
	dir := filepath.Join(home, ".gradle", "init.d")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "tentmaker-build-cache.gradle"), []byte(gradleInit(base)), 0o600)
}
