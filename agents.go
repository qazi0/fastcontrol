package main

import (
	"encoding/json"
	"errors"
	"os"
)

func rolloutActivity(line []byte) string {
	var record rolloutLine
	if json.Unmarshal(line, &record) != nil {
		return ""
	}
	var payload map[string]any
	if json.Unmarshal(record.Payload, &payload) != nil {
		return ""
	}
	if record.Type == "event_msg" && str(payload, "type") == "item_completed" {
		item := mapValue(payload["item"])
		switch str(item, "type") {
		case "AgentMessage":
			return clipPlain(joinTexts(arrayValue(item["content"]), "text"), 160)
		case "Reasoning":
			return clipPlain(joinTexts(arrayValue(item["summary_text"]), "text"), 160)
		case "CommandExecution":
			return "Running A Command"
		case "FileChange":
			return "Updating Files"
		}
	}
	if record.Type == "response_item" {
		switch str(payload, "type") {
		case "function_call", "custom_tool_call":
			return "Using " + clipPlain(str(payload, "name"), 100)
		case "reasoning":
			return clipPlain(joinTexts(arrayValue(payload["summary"]), "text"), 160)
		}
	}
	return ""
}

// Parentage comes from Codex's index, never from a matching folder or agent name.
func listSubagents(db, parent string) ([]ThreadInfo, error) {
	if parent == "" {
		return nil, errors.New("Choose A Parent Chat")
	}
	sourceMatch := `case when json_valid(source) then json_extract(source,'$.subagent.thread_spawn.parent_thread_id') end=` + sqlQuote(parent)
	query := threadSelect + ` where (id in (select child_thread_id from thread_spawn_edges where parent_thread_id=` + sqlQuote(parent) + `) or ` + sourceMatch + `) order by recency_at_ms desc limit 500`
	agents, err := queryThreads(db, query)
	if err != nil {
		// Earlier Codex databases recorded parentage only in source metadata.
		agents, err = queryThreads(db, threadSelect+` where `+sourceMatch+` order by recency_at_ms desc limit 500`)
	}
	filtered := agents[:0]
	for _, agent := range agents {
		if agent.Subagent || !codexFullChild(agent) {
			filtered = append(filtered, agent)
		}
	}
	agents = filtered
	for i := range agents {
		agents[i].Subagent = true
		agents[i].ParentID = parent
		if st, err := os.Stat(agents[i].Rollout); err == nil {
			status := tailStatus(agents[i].Rollout, st)
			agents[i].Running, agents[i].LastMsg, agents[i].LastErr, agents[i].TurnAt, agents[i].Activity = status.running, status.lastMsg, status.lastErr, status.turnAt, status.activity
		}
	}
	if agents == nil && err == nil {
		agents = []ThreadInfo{}
	}
	return agents, err
}

func codexFullChild(t ThreadInfo) bool {
	if t.Subagent {
		return false
	}
	switch t.Source {
	case "desktop", "app", "vscode", "cli":
		return true
	}
	switch t.Kind {
	case "main", "user", "app", "cli":
		return true
	}
	var source struct {
		Fork string `json:"forked_from_id"`
	}
	return json.Unmarshal([]byte(t.Source), &source) == nil && source.Fork != ""
}

func (h *Hub) isSubagent(id string) bool {
	if id == "" {
		return false
	}
	info, cached := h.threadInfo(id)
	if cached && info.Subagent {
		return true
	}
	if cached && codexFullChild(info) {
		return false
	}
	h.mu.Lock()
	s := h.sessions[id]
	h.mu.Unlock()
	if s != nil && s.info.Subagent {
		return true
	}
	if cached && info.Kind != "" {
		return false
	}
	rows, err := queryThreads(h.db, threadSelect+` where id=`+sqlQuote(id)+` limit 1`)
	if err == nil && len(rows) == 1 && rows[0].Subagent {
		return true
	}
	if err == nil && len(rows) == 1 && codexFullChild(rows[0]) {
		return false
	}
	data, err := sqliteJSON(h.db, `select child_thread_id from thread_spawn_edges where child_thread_id=`+sqlQuote(id)+` limit 1`)
	var edges []map[string]any
	return err == nil && json.Unmarshal(data, &edges) == nil && len(edges) == 1
}

func agentControl(op string) bool {
	switch op {
	case "send", "stop", "wake", "settings", "approve", "answer", "compact", "archive", "unarchive", "rename", "fork":
		return true
	}
	return false
}

func (h *Hub) handleAgents(c *Client, raw []byte) bool {
	var request struct{ Op, ID, Parent, Nonce string }
	if json.Unmarshal(raw, &request) != nil {
		return false
	}
	if request.Op != "agents" && request.Op != "agentOpen" {
		return false
	}
	if request.Op == "agentOpen" {
		h.mu.Lock()
		if c.subs == nil {
			c.subs = map[string]bool{}
		}
		c.subs[request.ID] = false
		h.mu.Unlock()
	}
	// Keep index reads ordered on this client's reader: Close must not overtake Open.
	func() {
		parent := request.ID
		if request.Op == "agentOpen" {
			parent = request.Parent
		}
		agents, err := listSubagents(h.db, parent)
		extra := map[string]any{"agents": agents}
		if err == nil && request.Op == "agentOpen" {
			var found bool
			for _, info := range agents {
				if info.ID != request.ID {
					continue
				}
				found = true
				if st, statErr := os.Stat(info.Rollout); statErr != nil || !st.Mode().IsRegular() {
					err = errors.New("Subagent Transcript Is Unavailable")
					break
				}
				h.mu.Lock()
				_, pending := c.subs[info.ID]
				if !h.clients[c] || !pending {
					h.mu.Unlock()
					return
				}
				s := h.sessions[info.ID]
				if s == nil {
					s = newSession(h, info)
					h.sessions[info.ID] = s
					go s.run()
				}
				if c.subs == nil {
					c.subs = map[string]bool{}
				}
				c.subs[info.ID] = true
				h.mu.Unlock()
				s.subscribe(c)
				extra = map[string]any{"agent": info}
				break
			}
			if !found {
				err = errors.New("Subagent Does Not Belong To This Chat")
			}
		}
		if err != nil && request.Op == "agentOpen" {
			h.mu.Lock()
			if subscribed, pending := c.subs[request.ID]; pending && !subscribed {
				delete(c.subs, request.ID)
			}
			h.mu.Unlock()
		}
		result := map[string]any{"t": "ack", "op": request.Op, "id": request.ID, "nonce": request.Nonce, "ok": err == nil}
		if err != nil {
			result["msg"] = err.Error()
		} else {
			for key, value := range extra {
				result[key] = value
			}
		}
		c.push(mustJSON(result))
	}()
	return true
}
