//go:build linux

package agent

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/resource-adjust/compute-platform/internal/telemetry"
)

func TestProcessGroupHelper(t *testing.T) {
	mode := os.Getenv("PLATFORM_TEST_PROCESS_TREE")
	if mode == "" {
		return
	}
	if mode == "parent" || mode == "parent-exits" {
		child := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
		child.Env = append(os.Environ(), "PLATFORM_TEST_PROCESS_TREE=child")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			panic(err)
		}
		if mode == "parent-exits" {
			os.Exit(0)
		}
		_ = child.Wait()
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	fmt.Fprintln(os.Stdout, listener.Addr().String())
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		connection.Close()
	}
}

func TestExitedParentAlsoCleansDescendants(t *testing.T) {
	e := NewExecutor(ExecutorConfig{}, telemetry.New())
	p, err := e.launch(os.Args[0], []string{"-test.run=^TestProcessGroupHelper$"}, []string{"PLATFORM_TEST_PROCESS_TREE=parent-exits"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.stop()
	deadline := time.Now().Add(3 * time.Second)
	endpoint := ""
	for time.Now().Before(deadline) {
		fields := strings.Fields(p.logs.Text())
		if len(fields) > 0 {
			endpoint = fields[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if endpoint == "" {
		t.Fatal("descendant did not publish its listener")
	}
	select {
	case <-p.done:
	case <-time.After(8 * time.Second):
		t.Fatal("crashed parent retained inherited pipe")
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", endpoint, 50*time.Millisecond)
		if err != nil {
			return
		}
		connection.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("untracked child survived its parent's exit")
}

func TestCancelTerminatesDescendantListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessGroupHelper$")
	cmd.Env = append(os.Environ(), "PLATFORM_TEST_PROCESS_TREE=parent")
	configureChild(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	address := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(out)
		if scanner.Scan() {
			address <- scanner.Text()
		}
	}()
	var endpoint string
	select {
	case endpoint = <-address:
	case <-time.After(5 * time.Second):
		t.Fatal("descendant did not start")
	}
	connection, err := net.DialTimeout("tcp", endpoint, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	connection.Close()
	cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("inherited pipe kept parent wait open")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		connection, err = net.DialTimeout("tcp", endpoint, 30*time.Millisecond)
		if err != nil {
			return
		}
		connection.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child service survived cancellation of its owner")
}
