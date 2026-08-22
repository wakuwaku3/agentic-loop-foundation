// Package legacyimport normalizes one read-only GitHub export into a bounded,
// secret-safe cutover manifest. It never talks to GitHub or writes back to it.
package legacyimport

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strings"
)

const Schema = "agentic-loop/legacy-export/v1"

type Export struct {
	Schema string  `json:"schema"`
	Issues []Issue `json:"issues"`
}

type Issue struct {
	Number   int       `json:"number"`
	Title    string    `json:"title"`
	Body     string    `json:"body"`
	Labels   []string  `json:"labels,omitempty"`
	Comments []Comment `json:"comments,omitempty"`
}

type Comment struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

type Disposition string

const (
	Import     Disposition = "import"
	Excluded   Disposition = "excluded"
	Quarantine Disposition = "quarantine"
)

type Entry struct {
	SourceNumber  int         `json:"source_number"`
	SourceDigest  string      `json:"source_digest"`
	Disposition   Disposition `json:"disposition"`
	Reason        string      `json:"reason,omitempty"`
	Title         string      `json:"title,omitempty"`
	ProblemSource string      `json:"problem_source,omitempty"`
}

type Manifest struct {
	Schema       string  `json:"schema"`
	ExportDigest string  `json:"export_digest"`
	Entries      []Entry `json:"entries"`
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----`),
	regexp.MustCompile(`\bgh[opusr]_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}\b`),
	regexp.MustCompile(`(?i)\b(?:api[_-]?key|token|password|secret)\s*[:=]\s*[^\s]{8,}`),
}

func Build(e Export, maxIssues, maxTextBytes int) (Manifest, error) {
	if e.Schema != Schema {
		return Manifest{}, errors.New("unsupported legacy export schema")
	}
	if maxIssues <= 0 || maxIssues > 10000 || len(e.Issues) > maxIssues {
		return Manifest{}, errors.New("legacy export issue limit exceeded")
	}
	if maxTextBytes <= 0 || maxTextBytes > 10<<20 {
		return Manifest{}, errors.New("invalid legacy export text limit")
	}
	seen := make(map[int]struct{}, len(e.Issues))
	entries := make([]Entry, 0, len(e.Issues))
	for _, issue := range e.Issues {
		if issue.Number <= 0 {
			return Manifest{}, errors.New("invalid legacy issue number")
		}
		if _, ok := seen[issue.Number]; ok {
			return Manifest{}, errors.New("duplicate legacy issue number")
		}
		seen[issue.Number] = struct{}{}
		canonical := canonicalText(issue)
		digest := sum(canonical)
		entry := Entry{SourceNumber: issue.Number, SourceDigest: digest}
		if len(canonical) > maxTextBytes {
			entry.Disposition, entry.Reason = Quarantine, "content-limit"
		} else if containsSecret(canonical) {
			entry.Disposition, entry.Reason = Quarantine, "secret-like-content"
		} else if reason := excludedReason(issue.Labels); reason != "" {
			entry.Disposition, entry.Reason = Excluded, reason
		} else {
			entry.Disposition = Import
			entry.Title = strings.TrimSpace(issue.Title)
			entry.ProblemSource = canonical
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].SourceNumber < entries[j].SourceNumber })
	var digestInput strings.Builder
	for _, entry := range entries {
		digestInput.WriteString(entry.SourceDigest)
		digestInput.WriteByte('\n')
	}
	return Manifest{Schema: "agentic-loop/legacy-import-manifest/v1", ExportDigest: sum(digestInput.String()), Entries: entries}, nil
}

func canonicalText(issue Issue) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(issue.Title))
	b.WriteString("\n\n")
	b.WriteString(strings.TrimSpace(issue.Body))
	for _, comment := range issue.Comments {
		b.WriteString("\n\n[comment by ")
		b.WriteString(strings.TrimSpace(comment.Author))
		b.WriteString("]\n")
		b.WriteString(strings.TrimSpace(comment.Body))
	}
	return strings.ReplaceAll(b.String(), "\r\n", "\n")
}

func containsSecret(value string) bool {
	for _, pattern := range secretPatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}

func excludedReason(labels []string) string {
	for _, label := range labels {
		switch strings.ToLower(strings.TrimSpace(label)) {
		case "agent:merged", "completed", "done":
			return "completed"
		case "agent:cancelled", "cancelled":
			return "cancelled"
		case "agent:duplicate", "duplicate", "agent:superseded":
			return "duplicate-or-superseded"
		}
	}
	return ""
}

func sum(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
