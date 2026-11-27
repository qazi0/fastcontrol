package main

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// antigravityRecordIsSubagent reports whether a summary record represents a
// delegated subagent rather than a top-level user conversation.
func antigravityRecordIsSubagent(rec antigravityRecord) bool {
	return rec.ParentConversationID != "" || rec.NestingDepth > 0
}

// antigravityGroupSubagents groups all subagent rows by their parent
// conversation ID.
func antigravityGroupSubagents(records []antigravityRecord) map[string][]antigravityRecord {
	grouped := map[string][]antigravityRecord{}
	for _, rec := range records {
		if !antigravityRecordIsSubagent(rec) || rec.ParentConversationID == "" || rec.ParentConversationID == rec.ID {
			continue
		}
		grouped[rec.ParentConversationID] = append(grouped[rec.ParentConversationID], rec)
	}
	return grouped
}

func antigravityAgentState(rec antigravityRecord, running bool) string {
	if running {
		return "running"
	}
	if rec.Killed {
		return "stopped"
	}
	upper := strings.ToUpper(strings.TrimSpace(rec.Status))
	switch {
	case upper == "CASCADE_RUN_STATUS_IDLE":
		return "completed"
	case strings.Contains(upper, "ERROR") || strings.Contains(upper, "FAIL"):
		return "failed"
	case strings.Contains(upper, "CANCEL") || strings.Contains(upper, "STOP"):
		return "stopped"
	case upper == "CASCADE_RUN_STATUS_RUNNING":
		// DB still said running, but stale-owner check determined it is not live.
		return "saved"
	case rec.StepCount > 0:
		return "completed"
	default:
		return "saved"
	}
}

func antigravityRecordTitle(rec antigravityRecord, fallback string) string {
	return antigravityFirstNonEmpty(
		rec.Title,
		rec.AnnotationTitle,
		rec.AgentName,
		clipPlain(rec.Preview, 120),
		fallback,
	)
}

// antigravityListAgents returns read-only subagent summaries belonging to
// parentID, sorted with running subagents first and newest Recency next.
func antigravityListAgents(records []antigravityRecord, parentID string, liveCheck func(string) bool) []ThreadInfo {
	if strings.TrimSpace(parentID) == "" {
		return []ThreadInfo{}
	}
	grouped := antigravityGroupSubagents(records)
	children := grouped[parentID]
	if len(children) == 0 {
		return []ThreadInfo{}
	}

	out := make([]ThreadInfo, 0, len(children))
	for _, rec := range children {
		running := rec.Running
		if liveCheck != nil && liveCheck(rec.ID) {
			running = true
		}
		agentName := antigravityFirstNonEmpty(rec.AgentName, rec.Title, rec.AnnotationTitle, "Subagent")
		title := antigravityRecordTitle(rec, "Subagent")
		state := antigravityAgentState(rec, running)
		source := rec.Source
		if source == "" {
			source = "antigravity"
		}
		out = append(out, ThreadInfo{
			Provider:   "antigravity",
			ID:         rec.ID,
			Title:      clipPlain(title, 240),
			Preview:    clipPlain(rec.Preview, 160),
			Cwd:        rec.Cwd,
			Source:     source,
			Kind:       "agent",
			Recency:    rec.Recency,
			Rollout:    rec.TranscriptPath,
			Active:     running,
			Running:    running,
			Subagent:   true,
			ParentID:   parentID,
			AgentName:  clipPlain(agentName, 240),
			AgentID:    rec.ID,
			AgentState: state,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Running != out[j].Running {
			return out[i].Running
		}
		if out[i].Recency != out[j].Recency {
			return out[i].Recency > out[j].Recency
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// antigravityIsSubagent reports whether id belongs to a subagent row in
// records, used for strict read-only ingress enforcement.
func antigravityIsSubagent(records []antigravityRecord, id string) bool {
	if strings.TrimSpace(id) == "" {
		return false
	}
	for _, rec := range records {
		if rec.ID == id {
			return antigravityRecordIsSubagent(rec)
		}
	}
	return false
}

// antigravitySubagentControlOp reports whether op mutates a conversation and
// must be rejected on Antigravity subagents.
func antigravitySubagentControlOp(op string) bool {
	return agentControl(op) || op == "pin" || op == "unpin"
}

// antigravityRejectSubagentControl enforces read-only ingress for Antigravity
// subagents across all control operations as well as pin and unpin.
func antigravityRejectSubagentControl(records []antigravityRecord, id, op string) error {
	if antigravityIsSubagent(records, id) && antigravitySubagentControlOp(op) {
		return errors.New("Subagent Chats Are Read Only. Use The Parent Chat To Direct The Agent")
	}
	return nil
}

// antigravityOpenAgent validates that agentID is a subagent owned by parentID
// and returns its read-only Meta and transcript turns.
func antigravityOpenAgent(records []antigravityRecord, parentID, agentID string, limit int) (Meta, []Turn, error) {
	view, err := antigravityOpenAgentTranscript(records, parentID, agentID, limit)
	if err != nil {
		return Meta{}, nil, err
	}
	return view.Meta, view.Turns, nil
}

// antigravityOpenAgentTranscript validates parentage and returns the complete
// read-only transcript view (including Meta, Turns, More, and Diffs).
func antigravityOpenAgentTranscript(records []antigravityRecord, parentID, agentID string, limit int) (*antigravityTranscript, error) {
	if strings.TrimSpace(parentID) == "" {
		return nil, errors.New("Choose A Parent Chat")
	}
	if strings.TrimSpace(agentID) == "" {
		return nil, errors.New("Subagent Does Not Belong To This Chat")
	}

	var agent *antigravityRecord
	for i := range records {
		if records[i].ID == agentID {
			agent = &records[i]
			break
		}
	}
	if agent == nil || !antigravityRecordIsSubagent(*agent) || agent.ParentConversationID != parentID || agent.ID == parentID {
		return nil, errors.New("Subagent Does Not Belong To This Chat")
	}

	state := antigravityAgentState(*agent, agent.Running)
	title := antigravityRecordTitle(*agent, "Subagent")

	var view *antigravityTranscript
	if agent.TranscriptPath != "" {
		if _, err := os.Stat(agent.TranscriptPath); err == nil {
			v, readErr := readAntigravityTranscriptWithOptions(agent.TranscriptPath, antigravityTranscriptOptions{
				Limit:    limit,
				FullPath: filepath.Join(filepath.Dir(agent.TranscriptPath), "transcript_full.jsonl"),
				BrainDir: agent.BrainDir,
				Cwd:      agent.Cwd,
				Title:    title,
			})
			if readErr != nil {
				return nil, readErr
			}
			view = v
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if view == nil {
		view = newAntigravityTranscript()
		view.Meta.Title = title
		view.Meta.Cwd = agent.Cwd
	}

	// Enforce read-only tail metadata regardless of transcript contents.
	view.Meta.Provider = "antigravity"
	view.Meta.ReadOnly = true
	view.Meta.Mode = "tail"
	view.Meta.Running = agent.Running
	view.Meta.AgentState = state
	if view.Meta.Title == "" {
		view.Meta.Title = title
	}
	if view.Meta.Cwd == "" {
		view.Meta.Cwd = agent.Cwd
	}
	return view, nil
}
