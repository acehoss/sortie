// Package issuekit provides shared issue normalization helpers for integration adapters.
package issuekit

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// SourceComment is the adapter-local staging shape for comment normalization.
type SourceComment struct {
	ID        string
	Author    string
	Body      string
	CreatedAt string
}

// NormalizeLabels lowercases labels in input order and always returns a non-nil slice.
func NormalizeLabels(in []string) []string {
	if in == nil {
		return []string{}
	}

	out := make([]string, len(in))
	for i, label := range in {
		out[i] = strings.ToLower(label)
	}
	return out
}

// ParsePriorityIntStrict parses a JSON integer literal and returns nil for all other JSON values.
func ParsePriorityIntStrict(raw json.RawMessage) *int {
	text := strings.TrimSpace(string(raw))
	if !isJSONIntLiteral(text) {
		return nil
	}
	return parseInt(text)
}

// ParsePriorityIntFromString parses a base-10 integer string and returns nil for invalid input.
func ParsePriorityIntFromString(s string) *int {
	text := strings.TrimSpace(s)
	if !isBase10Integer(text) {
		return nil
	}
	return parseInt(text)
}

// NormalizeComments maps adapter-local comment values to [domain.Comment] values.
func NormalizeComments(in []SourceComment) []domain.Comment {
	if in == nil {
		return []domain.Comment{}
	}

	out := make([]domain.Comment, len(in))
	for i, comment := range in {
		out[i] = domain.Comment{
			ID:        comment.ID,
			Author:    comment.Author,
			Body:      comment.Body,
			CreatedAt: comment.CreatedAt,
		}
	}
	return out
}

// MarkSelfComment returns text with marker appended on a separate
// trailing line so that comment-steering reads can recognise and drop
// orchestrator-authored comments. Empty marker or empty text returns
// text unchanged. A marker already present in text is not duplicated.
func MarkSelfComment(text, marker string) string {
	if marker == "" || text == "" {
		return text
	}
	if strings.Contains(text, marker) {
		return text
	}
	if strings.HasSuffix(text, "\n") {
		return text + marker
	}
	return text + "\n\n" + marker
}

// FilterSteeringComments returns the subset of in whose IDs are absent
// from seen, whose authors are absent from authorFilter
// (case-insensitive exact), and whose bodies do not contain selfMarker
// (when non-empty). Returns an empty slice when in is empty.
//
// Comments with an empty ID are always dropped: the watermark relies on
// the ID to deduplicate across turns, so an empty-ID comment would be
// re-delivered every turn indefinitely.
func FilterSteeringComments(in []domain.Comment, seen map[string]bool, authorFilter []string, selfMarker string) []domain.Comment {
	if len(in) == 0 {
		return nil
	}
	authors := make(map[string]bool, len(authorFilter))
	for _, a := range authorFilter {
		authors[strings.ToLower(a)] = true
	}
	out := make([]domain.Comment, 0, len(in))
	for _, c := range in {
		if c.ID == "" {
			continue
		}
		if seen[c.ID] {
			continue
		}
		if authors[strings.ToLower(c.Author)] {
			continue
		}
		if selfMarker != "" && strings.Contains(c.Body, selfMarker) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func isJSONIntLiteral(s string) bool {
	if !isBase10Integer(s) {
		return false
	}
	if s[0] == '-' {
		return len(s) <= 2 || s[1] != '0'
	}
	return len(s) <= 1 || s[0] != '0'
}

func isBase10Integer(s string) bool {
	if s == "" {
		return false
	}

	start := 0
	if s[0] == '-' {
		start = 1
	}
	if start == len(s) {
		return false
	}

	for i := start; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func parseInt(s string) *int {
	value, err := strconv.ParseInt(s, 10, strconv.IntSize)
	if err != nil {
		return nil
	}
	parsed := int(value)
	return &parsed
}
