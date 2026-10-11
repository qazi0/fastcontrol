package main

import (
	"encoding/json"
	"sync"
	"testing"
)

func TestHeadlessResponsePublishesBeforeApprovalReplay(t *testing.T) {
	h := NewHub(NewIPCClient(""), "")
	a := newHeadlessCodex(h)
	a.pending["resume"] = make(chan rpcResult, 1)
	a.receive([]byte(`{"id":"resume","result":{"thread":{"id":"chat","cwd":"/tmp","turns":[]},"model":"test","approvalPolicy":"on-request","sandbox":{"type":"readOnly"}}}`))
	a.receive([]byte(`{"id":7,"method":"item/commandExecution/requestApproval","params":{"threadId":"chat","turnId":"turn","command":"echo hello"}}`))
	a.mu.Lock()
	data := a.stateLocked("chat")
	a.mu.Unlock()
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	meta := desktopMeta(state, "")
	if meta.Model != "test" || len(meta.Requests) != 1 || meta.Requests[0].ID != "7" {
		t.Fatalf("lost resumed metadata or approval: %+v", meta)
	}
	a.receive([]byte(`{"method":"serverRequest/resolved","params":{"threadId":"chat","requestId":7}}`))
	if len(a.requests) != 0 {
		t.Fatal("resolved approval remained")
	}
}
func TestHeadlessStreamingAndCompletion(t *testing.T) {
	h := NewHub(NewIPCClient(""), "")
	a := newHeadlessCodex(h)
	messages := []string{
		`{"method":"thread/started","params":{"thread":{"id":"chat","turns":[]}}}`,
		`{"method":"turn/started","params":{"threadId":"chat","turn":{"id":"turn","status":"inProgress","startedAt":12,"items":[]}}}`,
		`{"method":"item/started","params":{"threadId":"chat","turnId":"turn","item":{"id":"message","type":"agentMessage","text":""}}}`,
		`{"method":"item/agentMessage/delta","params":{"threadId":"chat","turnId":"turn","itemId":"message","delta":"Hello "}}`,
		`{"method":"item/agentMessage/delta","params":{"threadId":"chat","turnId":"turn","itemId":"message","delta":"world"}}`,
	}
	for _, message := range messages {
		a.receive([]byte(message))
	}
	var state map[string]any
	_ = json.Unmarshal(a.stateLocked("chat"), &state)
	turn := desktopTurn("turn", desktopEntities(state)["turn"])
	if turn.Items[0].Text != "Hello world" || !desktopMeta(state, "").Running || turn.StartedAt != 12000 {
		t.Fatalf("streaming state: %+v", turn)
	}
	a.receive([]byte(`{"method":"turn/completed","params":{"threadId":"chat","turn":{"id":"turn","status":"completed","items":[]}}}`))
	_ = json.Unmarshal(a.stateLocked("chat"), &state)
	if desktopMeta(state, "").Running || desktopTurn("turn", desktopEntities(state)["turn"]).Items[0].Text != "Hello world" {
		t.Fatal("completion lost output or stayed running")
	}
}
func TestHeadlessConcurrentSnapshotAndDelta(t *testing.T) {
	a := newHeadlessCodex(NewHub(NewIPCClient(""), ""))
	a.receive([]byte(`{"method":"thread/started","params":{"thread":{"id":"chat","turns":[{"id":"turn","status":"inProgress","items":[{"id":"message","type":"agentMessage","text":""}]}]}}}`))
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := 0; i < 100; i++ {
			a.receive([]byte(`{"method":"item/agentMessage/delta","params":{"threadId":"chat","turnId":"turn","itemId":"message","delta":"x"}}`))
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 100; i++ {
			a.mu.Lock()
			data := a.stateLocked("chat")
			a.mu.Unlock()
			var value map[string]any
			if json.Unmarshal(data, &value) != nil {
				t.Error("partially published snapshot")
			}
		}
	}()
	workers.Wait()
}

func TestHeadlessTurnResponsePrecedesImmediateStop(t *testing.T) {
	a := newHeadlessCodex(NewHub(NewIPCClient(""), ""))
	a.threads["chat"] = map[string]any{"id": "chat", "turns": []any{}}
	a.pending["start"] = make(chan rpcResult, 1)
	a.pendingThreads["start"] = "chat"
	a.receive([]byte(`{"id":"start","result":{"turn":{"id":"turn","status":"inProgress","items":[]}}}`))
	<-a.pending["start"]
	if id := lastTurnIDJSON(a.threads["chat"]); id != "turn" {
		t.Fatalf("immediate Stop cannot find acknowledged turn: %q", id)
	}
}

func TestHeadlessStopRetriesCodexStartupRace(t *testing.T) {
	a := newHeadlessCodex(NewHub(NewIPCClient(""), ""))
	a.threads["chat"] = map[string]any{"turns": []any{map[string]any{"id": "turn", "status": "inProgress"}}}
	attempts := 0
	a.write = func(data []byte) error {
		var request map[string]any
		if err := json.Unmarshal(data, &request); err != nil {
			return err
		}
		attempts++
		response := map[string]any{"id": request["id"], "result": map[string]any{}}
		if attempts == 1 {
			delete(response, "result")
			response["error"] = map[string]any{"message": "no active turn to interrupt"}
		}
		a.receive(mustJSON(response))
		return nil
	}
	if err := a.interrupt("chat"); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("interrupt attempts: %d", attempts)
	}
}
