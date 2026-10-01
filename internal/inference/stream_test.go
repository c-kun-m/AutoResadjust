package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func content(w http.ResponseWriter, text string) {
	fmt.Fprintf(w, "data: {\"id\":\"test\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", text)
	w.(http.Flusher).Flush()
}
func finish(w http.ResponseWriter) {
	fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\ndata: [DONE]\n\n")
}

func runForward(t *testing.T, handler http.HandlerFunc, stream bool, p Policy, ctx context.Context) (*httptest.ResponseRecorder, string, Stats) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	defer upstream.Close()
	r := New(p)
	ticket, _ := r.Acquire(context.Background(), []string{"a"})
	req, _ := http.NewRequestWithContext(ctx, "POST", upstream.URL, nil)
	w := httptest.NewRecorder()
	outcome := Forward(w, req, upstream.Client(), stream, "pool", ticket, p)
	ticket.Finish(outcome)
	return w, outcome, r.Snapshot()[0]
}
func TestBothModesRequireActualProgress(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			p := DefaultPolicy()
			p.FirstToken = 40 * time.Millisecond
			p.Idle = 20 * time.Millisecond
			canceled := make(chan struct{})
			w, outcome, stats := runForward(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
				w.(http.Flusher).Flush()
				ticker := time.NewTicker(3 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-r.Context().Done():
						close(canceled)
						return
					case <-ticker.C:
						fmt.Fprint(w, ": keepalive\n\n")
						w.(http.Flusher).Flush()
					}
				}
			}, stream, p, context.Background())
			if outcome != "first_token_timeout" || stats.TTFTCount != 0 || stats.FirstTimeouts != 1 || !strings.Contains(w.Body.String(), "first_token_timeout") {
				t.Fatalf("heartbeat mistaken for generation: %s %+v %s", outcome, stats, w.Body.String())
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("upstream not canceled")
			}
		})
	}
}
func TestIdleTimeoutAndPrematureEOF(t *testing.T) {
	for _, eof := range []bool{false, true} {
		p := DefaultPolicy()
		p.Idle = 30 * time.Millisecond
		w, outcome, stats := runForward(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			content(w, "hi")
			if !eof {
				<-r.Context().Done()
			}
		}, true, p, context.Background())
		expected := "idle_timeout"
		if eof {
			expected = "upstream_error"
		}
		if outcome != expected || stats.TTFTCount != 1 || strings.Contains(w.Body.String(), "[DONE]") {
			t.Fatalf("failure disguised as success: %s %s", outcome, w.Body.String())
		}
	}
}
func TestNonStreamAggregationAndToolCalls(t *testing.T) {
	w, outcome, stats := runForward(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		content(w, "Hello ")
		content(w, "world")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call1","type":"function","function":{"name":"search","arguments":"{\"q\":"}}]}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`+"\n\n")
		finish(w)
	}, false, DefaultPolicy(), context.Background())
	if outcome != "completed" || stats.Completed != 1 || stats.TTFTCount != 1 {
		t.Fatalf("%s %+v %s", outcome, stats, w.Body.String())
	}
	var result struct {
		Model   string
		Choices []struct {
			Message struct {
				Content   string
				ToolCalls []struct {
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			}
		}
		Usage struct {
			Total int `json:"total_tokens"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Model != "pool" || result.Choices[0].Message.Content != "Hello world" || result.Usage.Total != 9 || result.Choices[0].Message.ToolCalls[0].Function.Arguments != `{"q":"x"}` {
		t.Fatalf("bad aggregation: %v %s", err, w.Body.String())
	}
}
func TestTotalDeadlineAndClientCancel(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if timeout {
			ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
		}
		defer cancel()
		_, outcome, _ := runForward(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			content(w, "hi")
			if !timeout {
				cancel()
			}
			<-r.Context().Done()
		}, true, DefaultPolicy(), ctx)
		expected := "canceled"
		if timeout {
			expected = "total_timeout"
		}
		if outcome != expected {
			t.Fatalf("%s, wanted %s", outcome, expected)
		}
	}
}
