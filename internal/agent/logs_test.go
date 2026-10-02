package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestProcessLogsRedactSplitWritesAndKeepIdentity(t *testing.T) {
	var output bytes.Buffer
	tail := &logTail{output: &output, node: "node-a", deployment: "dep-b", service: "node-a/model/dep-b", secrets: []string{"secret-lease-token"}}
	tail.Write([]byte("authorization: secret-lease-"))
	if tail.Text() != "" || output.Len() != 0 {
		t.Fatal("partial credential must not be exposed")
	}
	tail.Write([]byte("token\r\nfinal message"))
	tail.Flush()
	if strings.Contains(tail.Text(), "secret-") || strings.Contains(output.String(), "secret-") {
		t.Fatal("credential leaked")
	}
	if tail.Text() != "authorization: [REDACTED]\nfinal message\n" {
		t.Fatal(tail.Text())
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatal(output.String())
	}
	for _, line := range lines {
		var record map[string]string
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["service_id"] != "node-a/model/dep-b" || record["node_id"] != "node-a" || record["deployment_id"] != "dep-b" || record["event"] != "worker_output" || record["time"] == "" {
			t.Fatal(record)
		}
	}
}

func TestProcessLogsBoundLongLinesAndRecover(t *testing.T) {
	tail := &logTail{}
	tail.Write([]byte(strings.Repeat("x", 8193)))
	tail.Write([]byte("omitted remainder"))
	tail.Write([]byte("\nnext\n"))
	tail.Flush()
	if tail.Text() != "[oversize process log line omitted]\nnext\n" {
		t.Fatal(tail.Text())
	}
	tail.Write([]byte(strings.Repeat("y", 9000) + "\nlast\n"))
	if !strings.HasSuffix(tail.Text(), "[oversize process log line omitted]\nlast\n") {
		t.Fatal("same-write recovery failed")
	}
	for i := 0; i < 50; i++ {
		tail.Write([]byte(strings.Repeat("z", 1000) + "\n"))
	}
	if len(tail.Text()) > 32768 || len(tail.pending) > 8192 {
		t.Fatal("unbounded live log tail")
	}
}

func TestProcessExitDoesNotExposeUnfinishedCredential(t *testing.T) {
	tail := &logTail{secrets: []string{"secret-lease-token"}}
	tail.Write([]byte("authorization: secret-lease-"))
	tail.Flush()
	if tail.Text() != "authorization: [REDACTED]\n" {
		t.Fatal(tail.Text())
	}
}
