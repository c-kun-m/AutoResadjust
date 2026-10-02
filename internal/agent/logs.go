package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Keep both the node's short live tail and structured container stdout. A
// bounded line buffer handles split writes without retaining unlimited output.
type logTail struct {
	mu                        sync.Mutex
	text, pending             string
	discard                   bool
	node, deployment, service string
	output                    io.Writer
	secrets                   []string
}

func (l *logTail) redact(text string) string {
	for _, secret := range l.secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	return text
}

func (l *logTail) emit(line string) {
	line = l.redact(strings.TrimSuffix(line, "\r"))
	l.text += line + "\n"
	if len(l.text) > 32768 {
		l.text = l.text[len(l.text)-32768:]
	}
	if l.output != nil {
		data, _ := json.Marshal(map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "event": "worker_output", "node_id": l.node, "deployment_id": l.deployment, "service_id": l.service, "message": line})
		fmt.Fprintln(l.output, string(data))
	}
}

func (l *logTail) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	remaining := string(p)
	for len(remaining) > 0 {
		part, rest, complete := strings.Cut(remaining, "\n")
		if !l.discard {
			if len(l.pending)+len(part) > 8192 {
				// Omit the entire line, including subsequent writes, so a cut
				// through a credential can never expose its remaining bytes.
				l.emit("[oversize process log line omitted]")
				l.pending = ""
				l.discard = true
			} else {
				l.pending += part
			}
		}
		if complete {
			if !l.discard {
				l.emit(l.pending)
			}
			l.pending = ""
			l.discard = false
		}
		remaining = rest
	}
	return len(p), nil
}

func (l *logTail) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending != "" {
		// A process can exit between writes in the middle of a credential.
		// Omit its unfinished prefix instead of publishing a partial secret.
		for _, secret := range l.secrets {
			for n := len(secret) - 1; n >= 4; n-- {
				if strings.HasSuffix(l.pending, secret[:n]) {
					l.pending = l.pending[:len(l.pending)-n] + "[REDACTED]"
					break
				}
			}
		}
		l.emit(l.pending)
		l.pending = ""
	}
}

// Partial lines stay private until completed or the process exits: a partial
// write may contain only the first half of a credential awaiting redaction.
func (l *logTail) Text() string { l.mu.Lock(); defer l.mu.Unlock(); return l.text }

func (e *Executor) launchFor(service string, binary string, args, env []string) (*child, error) {
	return e.launchLogged(binary, args, env, &logTail{node: e.cfg.NodeID, service: service, deployment: e.current.work.DeploymentID, output: os.Stdout, secrets: []string{e.current.work.Token, os.Getenv("CONTROL_PLANE_TOKEN")}})
}
