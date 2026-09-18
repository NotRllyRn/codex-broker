package logbook

import (
	"net/url"
	"regexp"
	"strings"
)

var tokenPattern = regexp.MustCompile(`(?i)(bearer\s+)?(?:sk-[A-Za-z0-9_-]{16,}|wk1_[A-Za-z0-9_-]{20,})`)
var urlPattern = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)

var deniedKeys = map[string]bool{
	"authorization": true, "password": true, "token": true, "access_token": true,
	"refresh_token": true, "id_token": true, "csrf": true, "session": true,
	"authurl": true, "auth_url": true, "verificationurl": true, "verification_url": true,
	"usercode": true, "user_code": true, "callbackurl": true, "callback_url": true,
	"code": true, "state": true, "code_verifier": true, "code_challenge": true,
}

func Redact(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			if deniedKeys[strings.ToLower(key)] {
				result[key] = "[REDACTED]"
			} else {
				result[key] = Redact(item)
			}
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = Redact(item)
		}
		return result
	case string:
		value := strings.NewReplacer("\r", " ", "\n", " ").Replace(typed)
		if runes := []rune(value); len(runes) > 8192 {
			value = string(runes[:8192])
		}
		value = tokenPattern.ReplaceAllString(value, "[REDACTED]")
		return urlPattern.ReplaceAllStringFunc(value, sanitizeURL)
	default:
		return value
	}
}

func sanitizeURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "[REDACTED_URL]"
	}
	query := parsed.Query()
	for key := range query {
		query.Set(key, "[REDACTED]")
	}
	parsed.RawQuery, parsed.Fragment = query.Encode(), ""
	return parsed.String()
}
