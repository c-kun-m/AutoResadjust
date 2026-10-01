//go:build linux

package agent

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestProcessGroupHelper(t *testing.T) {
	mode := os.Getenv("PLATFORM_TEST_PROCESS_TREE")
	if mode == "" {
		return
	}
	if mode == "parent" {
		child := exec.Command(os.Args[0], "-test.run=^TestProcessGroupHelper$")
		child.Env = append(os.Environ(), "PLATFORM_TEST_PROCESS_TREE=child")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			panic(err)
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
