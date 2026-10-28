package commands

import "github.com/gravity-zero/mkvgo/matroska"

// rewriteReport is what reindex, reindex-inplace and retime report: text, or one JSON object under --json.
type rewriteReport struct {
	Source        string                   `json:"source"`
	Output        string                   `json:"output"`
	ShiftMs       map[uint64]int64         `json:"shift_ms,omitempty"`
	Skipped       []matroska.DamagedRange  `json:"skipped,omitempty"`
	Repaired      []matroska.RepairedRange `json:"repaired,omitempty"`
	Preexisting   []matroska.Issue         `json:"preexisting,omitempty"`
	RollbackDelta *rollbackDelta           `json:"rollback_delta,omitempty"`
}

// rollbackDelta is the delta file a --rollback-delta run wrote.
type rollbackDelta struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}
