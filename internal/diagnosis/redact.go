package diagnosis

import (
	"regexp"
	"strings"
)

var defaultRedactPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[a-zA-Z0-9\-._~+/]+=*`),
	regexp.MustCompile(`(?i)(password|passwd|pwd)\s*[=:]\s*\S+`),
	regexp.MustCompile(`(?i)(api[_-]?key|apikey)\s*[=:]\s*\S+`),
	regexp.MustCompile(`(?i)(secret|token)\s*[=:]\s*\S+`),
	regexp.MustCompile(`(?i)(mongodb|postgres|mysql|redis)://[^\s]+`),
}

// Redact scrubs sensitive data from a string.
func Redact(s string) string {
	for _, re := range defaultRedactPatterns {
		s = re.ReplaceAllStringFunc(s, func(match string) string {
			// Keep the key part, redact the value
			for _, prefix := range []string{"bearer ", "Bearer "} {
				if strings.HasPrefix(match, prefix) {
					return prefix + "[REDACTED]"
				}
			}
			idx := strings.IndexAny(match, "=:")
			if idx > 0 {
				return match[:idx+1] + "[REDACTED]"
			}
			return "[REDACTED]"
		})
	}
	return s
}

// RedactSlice redacts all strings in a slice.
func RedactSlice(ss []string) []string {
	result := make([]string, len(ss))
	for i, s := range ss {
		result[i] = Redact(s)
	}
	return result
}
