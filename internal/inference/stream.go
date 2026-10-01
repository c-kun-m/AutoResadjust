package inference

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const maxResponseBytes = 8 << 20

type event struct {
	data   []byte
	err    error
	status int
}

// readEvents never treats headers, comments or role-only chunks as progress.
// Closing the request context also interrupts transport reads and blocked sends.
func readEvents(ctx context.Context, client *http.Client, req *http.Request, out chan<- event) {
	send := func(e event) bool {
		select {
		case out <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		send(event{err: err})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		send(event{err: fmt.Errorf("upstream rejected inference (HTTP %d)", resp.StatusCode), status: resp.StatusCode})
		return
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		send(event{err: errors.New("upstream must provide a streaming chat response")})
		return
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var data bytes.Buffer
	total := 0
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line) + 1
		if total > maxResponseBytes {
			send(event{err: errors.New("upstream response exceeds 8 MiB")})
			return
		}
		if line == "" {
			if data.Len() > 0 {
				payload := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
				if !send(event{data: append([]byte(nil), payload...)}) {
					return
				}
				if string(payload) == "[DONE]" {
					return
				}
				data.Reset()
			}
		} else if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			data.WriteString(value)
			data.WriteByte('\n')
			if data.Len() > 1<<20 {
				send(event{err: errors.New("upstream event exceeds 1 MiB")})
				return
			}
		}
	}
	if err := scanner.Err(); err != nil {
		send(event{err: err})
	} else {
		send(event{err: errors.New("upstream closed before a complete response")})
	}
}

type chunk struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string      `json:"role"`
			Content   string      `json:"content"`
			Refusal   string      `json:"refusal"`
			Reasoning string      `json:"reasoning_content"`
			Tools     []toolDelta `json:"tool_calls"`
		} `json:"delta"`
		Finish *string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
	Error json.RawMessage `json:"error"`
}
type toolDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type aggregate struct {
	id                          string
	created                     int64
	content, refusal, reasoning strings.Builder
	tools                       []toolDelta
	finish                      *string
	usage                       json.RawMessage
}

func (a *aggregate) accept(c chunk) (bool, error) {
	if len(c.Error) > 0 && string(c.Error) != "null" {
		return false, errors.New("upstream reported a stream error")
	}
	if len(c.Choices) > 1 || len(c.Choices) == 1 && c.Choices[0].Index != 0 {
		return false, errors.New("only a single completion choice is supported")
	}
	if a.id == "" {
		a.id = c.ID
		a.created = c.Created
	}
	if len(c.Usage) > 0 && string(c.Usage) != "null" {
		a.usage = c.Usage
	}
	progress := false
	for _, choice := range c.Choices {
		d := choice.Delta
		progress = d.Content != "" || d.Refusal != "" || d.Reasoning != ""
		a.content.WriteString(d.Content)
		a.refusal.WriteString(d.Refusal)
		a.reasoning.WriteString(d.Reasoning)
		if choice.Finish != nil {
			a.finish = choice.Finish
		}
		for _, t := range d.Tools {
			if t.Index < 0 || t.Index > 127 {
				return false, errors.New("invalid tool call index")
			}
			for len(a.tools) <= t.Index {
				a.tools = append(a.tools, toolDelta{Index: len(a.tools)})
			}
			v := &a.tools[t.Index]
			if t.ID != "" {
				v.ID = t.ID
			}
			if t.Type != "" {
				v.Type = t.Type
			}
			v.Function.Name += t.Function.Name
			v.Function.Arguments += t.Function.Arguments
			progress = progress || t.Function.Name != "" || t.Function.Arguments != ""
		}
	}
	return progress, nil
}
func (a *aggregate) response(model string) map[string]any {
	message := map[string]any{"role": "assistant", "content": a.content.String()}
	if a.refusal.Len() > 0 {
		message["refusal"] = a.refusal.String()
	}
	if a.reasoning.Len() > 0 {
		message["reasoning_content"] = a.reasoning.String()
	}
	if len(a.tools) > 0 {
		calls := []any{}
		for _, t := range a.tools {
			calls = append(calls, map[string]any{"id": t.ID, "type": t.Type, "function": t.Function})
		}
		message["tool_calls"] = calls
		if a.content.Len() == 0 {
			message["content"] = nil
		}
	}
	result := map[string]any{"id": a.id, "object": "chat.completion", "created": a.created, "model": model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": a.finish}}}
	if len(a.usage) > 0 {
		result["usage"] = a.usage
	}
	return result
}

func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message, "type": "inference_error", "code": code}})
}

// Forward requests streaming upstream even for non-streaming clients. This lets
// both API modes enforce real first-token and inter-token deadlines. No replay
// or replica switch is attempted after dispatch.
func Forward(w http.ResponseWriter, req *http.Request, client *http.Client, stream bool, model string, ticket *Ticket, policy Policy) (outcome string) {
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(ctx)
	events := make(chan event)
	go readEvents(ctx, client, req, events)
	timer := time.NewTimer(policy.FirstToken)
	defer timer.Stop()
	progressDeadline := time.Now().Add(policy.FirstToken)
	first := false
	wrote := false
	a := &aggregate{}
	rc := http.NewResponseController(w)
	defer rc.SetWriteDeadline(time.Time{})
	write := func(data []byte) error {
		deadline := time.Now().Add(policy.Idle)
		if ctx.Err() != nil {
			deadline = time.Now().Add(250 * time.Millisecond)
		} else if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = rc.SetWriteDeadline(deadline)
		if stream {
			if !wrote {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("X-Accel-Buffering", "no")
			}
			wrote = true
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return err
			}
			return rc.Flush()
		}
		w.Header().Set("Content-Type", "application/json")
		wrote = true
		_, err := w.Write(data)
		return err
	}
	fail := func(status int, code, message string) string {
		if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "canceled"
		}
		if wrote && stream {
			payload, _ := json.Marshal(map[string]any{"error": map[string]string{"message": message, "type": "inference_error", "code": code}})
			_ = write(payload)
		} else {
			_ = rc.SetWriteDeadline(time.Now().Add(time.Second))
			WriteError(w, status, code, message)
		}
		return code
	}
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fail(504, "total_timeout", "inference total deadline exceeded")
			}
			return "canceled"
		case <-timer.C:
			if !first {
				return fail(504, "first_token_timeout", "no generated content before first-token deadline")
			}
			return fail(504, "idle_timeout", "generation made no progress before idle deadline")
		case e := <-events:
			if ctx.Err() != nil {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return fail(504, "total_timeout", "inference total deadline exceeded")
				}
				return "canceled"
			}
			if time.Now().After(progressDeadline) {
				if !first {
					return fail(504, "first_token_timeout", "no generated content before first-token deadline")
				}
				return fail(504, "idle_timeout", "generation made no progress before idle deadline")
			}
			if e.err != nil {
				status := e.status
				if status < 400 || status > 599 {
					status = 502
				}
				return fail(status, "upstream_error", e.err.Error())
			}
			if string(e.data) == "[DONE]" {
				if a.finish == nil {
					return fail(502, "upstream_error", "stream ended without a finish reason")
				}
				data := e.data
				if !stream {
					data, _ = json.Marshal(a.response(model))
				}
				if err := write(data); err != nil {
					return "client_write_error"
				}
				return "completed"
			}
			var c chunk
			if err := json.Unmarshal(e.data, &c); err != nil {
				return fail(502, "upstream_error", "invalid upstream chat event")
			}
			progress, err := a.accept(c)
			if err != nil {
				return fail(502, "upstream_error", err.Error())
			}
			if progress {
				if !first {
					ticket.FirstToken()
					first = true
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(policy.Idle)
				progressDeadline = time.Now().Add(policy.Idle)
			}
			if stream {
				var body map[string]json.RawMessage
				_ = json.Unmarshal(e.data, &body)
				if body == nil {
					return fail(502, "upstream_error", "upstream event must be an object")
				}
				body["model"], _ = json.Marshal(model)
				payload, _ := json.Marshal(body)
				if err := write(payload); err != nil {
					return "client_write_error"
				}
			}
		}
	}
}
