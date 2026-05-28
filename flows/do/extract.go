package main

import (
	"encoding/json"
	"regexp"
	"strings"

	flowsdk "djabi.dev/go/flow_sdk"
)

// extract.go parses durable artifact content out of an agent turn's free-text
// last message. The flow — not the agent — is the artifact writer (it records
// the parsed value via the SDK setters), so these parsers turn the agent's
// prose/JSON into the typed wire values. Each falls back to a safe default so a
// step always produces its artifact and the flow keeps progressing.

// coverageRe matches a "COVERAGE: <rating>" line the coverage step asks the
// agent to end with. Case-insensitive; the last match wins.
var coverageRe = regexp.MustCompile(`(?i)COVERAGE:\s*(adequate|insufficient|none)`)

// extractCoverage parses the coverage rating from the agent's message. Defaults
// to "adequate" when no explicit rating is found — the turn ran and addressed
// coverage; the value is informational and must not block finalization.
func extractCoverage(msg string) flowsdk.TestCoverage {
	matches := coverageRe.FindAllStringSubmatch(msg, -1)
	if len(matches) == 0 {
		return flowsdk.CoverageAdequate
	}
	switch strings.ToLower(matches[len(matches)-1][1]) {
	case "insufficient":
		return flowsdk.CoverageInsufficient
	case "none":
		return flowsdk.CoverageNone
	default:
		return flowsdk.CoverageAdequate
	}
}

// jsonBlockRe matches a fenced ```json ... ``` block. The last block wins.
var jsonBlockRe = regexp.MustCompile("(?s)```json\\s*(.*?)```")

// inspectionPayload is the structured verdict the inspection step asks the agent
// to emit as a JSON block, plus optional follow-up suggestions.
type inspectionPayload struct {
	Verdict      flowsdk.Verdict      `json:"verdict"`
	Quality      flowsdk.Quality      `json:"quality"`
	Completeness flowsdk.Completeness `json:"completeness"`
	Summary      flowsdk.Markdown     `json:"summary"`
	Tags         []flowsdk.TagName    `json:"tags"`
	Suggestions  []suggestionPayload  `json:"suggestions"`
}

// suggestionPayload is one proposed follow-up item from inspection.
type suggestionPayload struct {
	Title       string           `json:"title"`
	Type        flowsdk.ItemType `json:"type"`
	Description flowsdk.Markdown `json:"description"`
	Priority    flowsdk.Priority `json:"priority"`
	Rationale   flowsdk.Markdown `json:"rationale"`
	Key         string           `json:"key"`
}

// extractInspection parses the inspection verdict (and any suggestions) from the
// agent's message. When no parseable JSON block is present it falls back to a
// "concerns" verdict carrying the raw message as the summary, so the inspection
// artifact is still produced (the item finalizes) while signalling the parse gap.
func extractInspection(msg string, by flowsdk.AgentName) (flowsdk.Inspection, []flowsdk.ItemSuggestion) {
	insp := flowsdk.Inspection{InspectedBy: by}
	var sugs []flowsdk.ItemSuggestion

	// Pick the LAST parseable JSON block with a verdict, so an agent that echoes
	// the template block before emitting its real verdict doesn't trip us up.
	var p inspectionPayload
	parsed := false
	for _, m := range jsonBlockRe.FindAllStringSubmatch(msg, -1) {
		var cand inspectionPayload
		if json.Unmarshal([]byte(strings.TrimSpace(m[1])), &cand) == nil && cand.Verdict != "" {
			p, parsed = cand, true
		}
	}
	if parsed {
		insp.Verdict = p.Verdict
		insp.Quality = p.Quality
		insp.Completeness = p.Completeness
		insp.Summary = p.Summary
		insp.Tags = p.Tags
		for _, s := range p.Suggestions {
			if s.Title == "" {
				continue
			}
			sugs = append(sugs, flowsdk.ItemSuggestion{
				Source:      flowsdk.StepName(flowsdk.ArtifactInspection),
				Type:        s.Type,
				Title:       s.Title,
				Description: s.Description,
				Priority:    s.Priority,
				Rationale:   s.Rationale,
				Key:         s.Key,
			})
		}
		return insp, sugs
	}

	insp.Verdict = flowsdk.VerdictConcerns
	insp.Summary = flowsdk.Markdown(strings.TrimSpace(msg))
	if insp.Summary == "" {
		insp.Summary = "inspection produced no parseable verdict"
	}
	return insp, sugs
}
