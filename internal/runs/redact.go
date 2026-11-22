package runs

import (
	"bytes"
	"regexp"
	"strings"
)

// Mask is what Redact puts in place of a secret: the same "***" the
// actions/runner writes for a masked value.
const Mask = "***"

// secretPatterns match values that look like credentials. The runner masks
// the secrets it knows (repo secrets, GITHUB_TOKEN, ::add-mask:: values)
// before it writes a log; these catch what it can't know about, and what
// reaches Tentmaker's own logs (check-run output, the runner's diag logs).
var secretPatterns = []*regexp.Regexp{
	// GitHub tokens: personal (ghp_), OAuth (gho_), user-to-server (ghu_),
	// server-to-server, the app installation and GITHUB_TOKEN kind (ghs_),
	// refresh (ghr_), and fine-grained personal tokens (github_pat_).
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	// GitLab personal, runner and other tokens.
	regexp.MustCompile(`\bgl(?:pat|rt|cbt|dt|ft|imt|oas|ptt|soat)-[A-Za-z0-9_\-]{20,}`),
	// A JIT runner config handed over in an environment line.
	regexp.MustCompile(`(ACTIONS_RUNNER_INPUT_JITCONFIG\s*[=:]\s*)\S+`),
	// JWTs, such as ACTIONS_RUNTIME_TOKEN and OIDC tokens.
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`),
	// Long base64 runs: a GitHub JIT config is base64 JSON (".runner",
	// ".credentials", ".credentials_rsaparams"), far longer than this.
	regexp.MustCompile(`[A-Za-z0-9+/]{120,}={0,2}`),
	// Credentials in HTTP headers and URLs.
	regexp.MustCompile(`(?i)(authorization\s*:\s*(?:bearer|token|basic)\s+)[^\s"']+`),
	regexp.MustCompile(`(?i)(private-token\s*:\s*)[^\s"']+`),
	regexp.MustCompile(`(?i)(x-access-token:)[^@\s"']+`),
	regexp.MustCompile(`(https?://[^/\s:@"']+:)[^@\s"'/]+` + "(?:@)"),
	// AWS access key ids.
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	// Chat webhooks: the path is the secret.
	regexp.MustCompile(`(https://hooks\.slack\.com/services/)[A-Za-z0-9/_\-]+`),
	regexp.MustCompile(`(https://(?:discord|discordapp)\.com/api/webhooks/)[A-Za-z0-9/_\-]+`),
}

// Redact returns data with every value that looks like a token or a JIT
// config replaced by Mask, and every exact value in extra too (a grant's
// secret, when the caller still has it). It never returns data itself, so
// the caller may keep the original.
func Redact(data []byte, extra ...[]byte) []byte {
	out := bytes.Clone(data)
	for _, s := range extra {
		if len(s) >= 4 {
			out = bytes.ReplaceAll(out, s, []byte(Mask))
		}
	}
	for _, re := range secretPatterns {
		if strings.HasSuffix(re.String(), "(?:@)") {
			out = re.ReplaceAll(out, []byte("${1}"+Mask+"@"))
		} else if re.NumSubexp() > 0 {
			out = re.ReplaceAll(out, []byte("${1}"+Mask))
		} else {
			out = re.ReplaceAll(out, []byte(Mask))
		}
	}
	return out
}

// RedactString is Redact for a string.
func RedactString(s string) string { return string(Redact([]byte(s))) }
