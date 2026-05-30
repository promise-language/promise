package main

import (
	"fmt"
	"regexp"
	"strings"

	flowsdk "djabi.dev/go/flow_sdk"
)

// buildCommitMessage formats the commit message used by the do-flow's commit
// step. Subject is "<ID>: <Title>"; the body ends with a Co-Authored-By trailer
// naming the model that did the work (Item.Model), matching the repo's
// convention for agent-authored commits (CLAUDE.md: "Co-Authored-By: Claude Opus
// 4.8 <noreply@anthropic.com>"). The "why" lives in the item's plan/summary on
// the tracker, linked by the id.
func buildCommitMessage(it *flowsdk.Item) string {
	subject := string(it.ID) + ": " + it.Title
	trailer := "Co-Authored-By: " + modelDisplayName(it.Model) + " <noreply@anthropic.com>"
	return subject + "\n\n" + trailer
}

// dateSuffix strips a trailing "-YYYYMMDD" snapshot pin from model ids like
// "claude-haiku-4-5-20251001" so the display name doesn't carry the date.
var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// modelDisplayName maps a ModelName to the human-readable form used in repo
// commit trailers. Empty maps to bare "Claude"; an unrecognized id is wrapped as
// "Claude (<raw>)" so it stays visibly unmapped rather than silently mislabeled.
func modelDisplayName(m flowsdk.ModelName) string {
	raw := strings.TrimSpace(string(m))
	if raw == "" {
		return "Claude"
	}
	key := dateSuffix.ReplaceAllString(strings.ToLower(raw), "")
	switch key {
	case "opus":
		return "Claude Opus"
	case "sonnet":
		return "Claude Sonnet"
	case "haiku":
		return "Claude Haiku"
	}
	// claude-<family>-<major>-<minor>
	if strings.HasPrefix(key, "claude-") {
		parts := strings.Split(key, "-")
		if len(parts) == 4 {
			family := parts[1]
			major := parts[2]
			minor := parts[3]
			switch family {
			case "opus", "sonnet", "haiku":
				return fmt.Sprintf("Claude %s %s.%s", capitalize(family), major, minor)
			}
		}
	}
	return "Claude (" + raw + ")"
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
