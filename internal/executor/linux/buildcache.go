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

// hostBazelRC is the host's system bazelrc. The job unit binds the job's
// own copy over it (jobBazelRC), so that copy keeps the host's lines.
var hostBazelRC = "/etc/bazel.bazelrc"

// jobBazelRC is the file in the job dir that the job unit binds over
// /etc/bazel.bazelrc.
const jobBazelRC = "bazel.bazelrc"

// bazelRC is the job's system bazelrc: the host's own lines, then the
// device's cache. Bazel reads the system rc before the workspace and home
// ones, so a remote cache or executor the repo sets still wins (review
// first-F2), and a --remote_cache on the command line wins over both.
func bazelRC(host []byte, base string) string {
	var b strings.Builder
	if len(host) > 0 {
		b.Write(host)
		if host[len(host)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	b.WriteString("# Written by Tentmaker: this device's build cache for the job's repo.\n" +
		"# It names the job by its connection, so there is no credential here.\n" +
		"build --remote_cache=" + strings.TrimSuffix(base, "/") + "\n")
	return b.String()
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

// writeBuildCacheFiles writes the job's system bazelrc into dir and the
// Gradle init script into its home. dir is root-owned until prepareJob
// hands it over.
func writeBuildCacheFiles(dir, base string) error {
	host, err := os.ReadFile(hostBazelRC)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, jobBazelRC), []byte(bazelRC(host, base)), 0o644); err != nil {
		return err
	}
	home := filepath.Join(dir, "home")
	initd := filepath.Join(home, ".gradle", "init.d")
	if err := os.MkdirAll(initd, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(initd, "tentmaker-build-cache.gradle"), []byte(gradleInit(base)), 0o600)
}
