package domain

import (
	"regexp"
	"strings"
)

type Client struct {
	ID                 int64
	Slug               string
	Name               string
	AllowedTypes       []string
	MaxFileBytes       int64
	QuotaBytes         int64
	RateLimitPerMinute int
}

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

func ValidSlug(slug string) bool {
	return slugPattern.MatchString(slug)
}

func (c Client) Allows(contentType string) bool {
	normalized := NormalizeContentType(contentType)

	for _, pattern := range c.AllowedTypes {
		switch {
		case pattern == "*", pattern == normalized:
			return true
		case strings.HasSuffix(pattern, "/*") && strings.HasPrefix(normalized, strings.TrimSuffix(pattern, "*")):
			return true
		}
	}

	return false
}

func ValidTypePattern(pattern string) bool {
	if pattern == "*" {
		return true
	}

	if prefix, ok := strings.CutSuffix(pattern, "/*"); ok {
		for contentType := range catalog {
			if strings.HasPrefix(contentType, prefix+"/") {
				return true
			}
		}

		return false
	}

	_, ok := catalog[pattern]

	return ok
}
