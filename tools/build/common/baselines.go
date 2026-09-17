package common

// The ratcheted terms `bin/run` judges against, and nothing that moves them.
//
// A baseline is the best a metric has been, and it is READ here and written
// elsewhere. This project used to carry its own `bin/commitgate` to advance
// them on a complete passing run; it was deleted once nothing invoked it —
// no workflow, no hook, no schedule ever named it — and advancing a baseline
// is a workspace concern, uniform across every managed repository, rather than
// a tool each project reimplements.
//
// So what remains here is the reading half: the shape of the file, how to load
// it, and what its directions mean. `judge` (judge.go) applies them, and moves
// nothing — see its own note on why an incomplete run is still judged but may
// never lower a floor.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Baseline represents a single ratcheted metric. Three states:
//   - Enforced: Direction != "" && Value != nil — ratchet-checked
//   - Pending:  Direction != "" && Value == nil — value populated when one is set
//   - Informational: Type == "informational" — tracked but not enforced
type Baseline struct {
	Value     *float64 `json:"value,omitempty"`     // nil = not yet set (Pending)
	Direction string   `json:"direction,omitempty"` // "up", "down", "exact"; absent = Informational
	Updated   string   `json:"updated,omitempty"`   // YYYY-MM-DD
	Type      string   `json:"type,omitempty"`      // "informational" for auto-created entries
}

// Baselines maps platform → metric name → baseline.
type Baselines map[string]map[string]Baseline

const baselinesFile = "tools/gates/baselines.json"

// LoadBaselines reads baselines.json from the repo root.
func LoadBaselines(root string) (Baselines, error) {
	path := filepath.Join(root, baselinesFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read baselines: %w", err)
	}
	var b Baselines
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parse baselines: %w", err)
	}
	return b, nil
}

// checkRatchet returns true if the new value satisfies the ratchet direction.
func checkRatchet(direction string, baseline, actual float64) bool {
	switch direction {
	case "up":
		return actual >= baseline
	case "down":
		return actual <= baseline
	case "exact":
		return actual == baseline
	default:
		return true
	}
}

// ratchetVerb renders a direction for a person reading a refusal.
func ratchetVerb(direction string) string {
	switch direction {
	case "up":
		return "must not decrease"
	case "down":
		return "must not increase"
	case "exact":
		return "must not change"
	default:
		return "is informational"
	}
}
