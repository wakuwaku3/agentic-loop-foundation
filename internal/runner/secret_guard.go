package runner

import (
	"errors"
	"regexp"
	"strings"
)

var secretName = regexp.MustCompile(`(?i)(token|secret|password|private[_-]?key|credential|api[_-]?key)`)
var secretValue = regexp.MustCompile(`(?i)(bearer\s+[A-Za-z0-9._~+/=-]{12,}|-----BEGIN [A-Z ]+PRIVATE KEY-----|gh[pousr]_[A-Za-z0-9]{20,})`)

func GuardCommand(argv []string, env []string) error {
	for _, arg := range argv {
		if secretValue.MatchString(arg) {
			return errors.New("secret-like value in process argv")
		}
	}
	for _, item := range env {
		key, value, _ := strings.Cut(item, "=")
		if secretName.MatchString(key) {
			return errors.New("secret-like environment variable is forbidden")
		}
		if secretValue.MatchString(value) {
			return errors.New("secret-like value in process environment")
		}
	}
	return nil
}

// GuardEnvironment is the stricter runner boundary: only explicitly declared
// harmless variables cross into a child process, and values are inspected too.
func GuardEnvironment(env []string, allowlist map[string]bool) error {
	for _, item := range env {
		key, value, _ := strings.Cut(item, "=")
		if !allowlist[key] {
			return errors.New("environment variable is not allowlisted")
		}
		if secretName.MatchString(key) || secretValue.MatchString(value) {
			return errors.New("secret-like environment entry")
		}
	}
	return nil
}
func RedactLog(value string) string { return secretValue.ReplaceAllString(value, "[REDACTED]") }
