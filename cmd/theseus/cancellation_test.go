package main

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"theseus/internal/deploy"
)

func TestReceiveProcess(t *testing.T) {
	if os.Getenv("THESEUS_TEST_PROCESS") != "1" {
		return
	}
	os.Args = []string{"theseus", "--home", os.Getenv("THESEUS_TEST_HOME"), "receive", "--repository", "owner/site", "--commit", strings.Repeat("a", 40), "--sequence", "1"}
	main()
}

func TestSIGTERMInterruptsInheritedStdin(t *testing.T) {
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestReceiveProcess$")
	cmd.Env = append(os.Environ(), "THESEUS_TEST_PROCESS=1", "THESEUS_TEST_HOME="+home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "Preparing release") {
				close(ready)
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not begin upload")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled deployment returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not interrupt stdin")
	}
	m := deploy.Manager{Home: home}
	unlock, err := m.Lock()
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
