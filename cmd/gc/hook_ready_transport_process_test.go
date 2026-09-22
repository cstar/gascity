//go:build cmd_gc_process

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestHookReadyTransportLifetimeAndReadReuse(t *testing.T) {
	var reads atomic.Int32
	reader := newHookReadyReader(func(string) ([]beads.Bead, map[string]readyLeg, error) {
		reads.Add(1)
		return []beads.Bead{{ID: "task", Status: "open", Assignee: "session"}}, nil, nil
	})
	endpoint, closeReader, err := startHookReadyServer(reader)
	if err != nil {
		t.Fatal(err)
	}
	defer closeReader()
	for i := 0; i < 2; i++ {
		got, err := readHookReadyRemote(endpoint, readyOpts{assignee: "session", limit: 1})
		if err != nil || len(got) != 1 || got[0].ID != "task" {
			t.Fatalf("rows=%+v err=%v", got, err)
		}
	}
	if reads.Load() != 1 {
		t.Fatalf("read underlying stores %d times", reads.Load())
	}
	closeReader()
	if _, err := os.Stat(filepath.Dir(endpoint)); !os.IsNotExist(err) {
		t.Fatalf("reader directory survives close: %v", err)
	}
	if _, err := readHookReadyRemote(endpoint, readyOpts{}); err == nil {
		t.Fatal("closed reader remained usable")
	}
}

func TestHookReadyCleanupDisconnectsAnActiveClient(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	reader := newHookReadyReader(func(string) ([]beads.Bead, map[string]readyLeg, error) {
		close(entered)
		<-release
		return []beads.Bead{}, nil, nil
	})
	endpoint, cleanup, err := startHookReadyServer(reader)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	result := make(chan error, 1)
	go func() { _, err := readHookReadyRemote(endpoint, readyOpts{}); result <- err }()
	awaitClose(t, entered, "active reader request")
	cleanup()
	close(release)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("active client did not observe cleanup")
		}
	case <-time.After(hangBudget):
		t.Fatal("active client stuck after cleanup")
	}
}

func TestHookReadyReaderChildHasAnAutonomousLease(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "gcr-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	previousTimeout := hookWorkQueryTimeout
	hookWorkQueryTimeout = testutil.ExecRaceTimeout
	defer func() { hookWorkQueryTimeout = previousTimeout }()
	_, err = shellWorkQueryWithEnv(shellquote.Join([]string{exe, "-test.run=^TestHookReadyLeaseHelper$"}), "", append(os.Environ(), "GC_TEST_HOOK_READER_ENDPOINT="+filepath.Join(dir, "reader.sock")))
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 124 {
		t.Fatalf("lease did not terminate isolated reader: %v", err)
	}
}

func TestHookReadyLeaseHelper(t *testing.T) {
	endpoint := os.Getenv("GC_TEST_HOOK_READER_ENDPOINT")
	if endpoint == "" {
		t.Skip("reader child helper")
	}
	hookWorkQueryTimeout = 50 * time.Millisecond
	if err := serveNativeHookReadyEndpoint(endpoint, io.Discard); err != nil {
		t.Fatal(err)
	}
	t.Fatal("reader returned without lease expiry")
}

func TestNativeHookReaderScriptCanRetrySameEndpoint(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "gcr-retry-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	shim := filepath.Join(dir, "gc")
	script := "#!/bin/sh\nexec " + shellquote.Quote(exe) + " -test.run=^TestHookReadyTransportCommandHelper$ -- \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	endpoint := filepath.Join(dir, "reader.sock")
	query, err := hookQueryWithReadyReader("sh -c 'gc ready --json'", endpoint, shim, dir)
	if err != nil {
		t.Fatal(err)
	}
	previousTimeout := hookWorkQueryTimeout
	hookWorkQueryTimeout = testutil.ExecRaceTimeout
	defer func() { hookWorkQueryTimeout = previousTimeout }()
	for i := 0; i < 2; i++ {
		out, err := shellWorkQueryWithEnv(query, "", nil)
		if err != nil || !strings.Contains(out, `"task"`) {
			t.Fatalf("attempt %d: %s %v", i, out, err)
		}
		if _, err := os.Stat(endpoint); !os.IsNotExist(err) {
			t.Fatalf("socket survives attempt %d: %v", i, err)
		}
	}
}

func TestHookReadyTransportCommandHelper(t *testing.T) {
	args := os.Args
	split := -1
	for i, arg := range args {
		if arg == "--" {
			split = i
			break
		}
	}
	if split < 0 {
		t.Skip("reader CLI child helper")
	}
	args = args[split+1:]
	for i, arg := range args {
		if arg == "--serve-hook-reader" && i+1 < len(args) {
			reader := newHookReadyReader(func(string) ([]beads.Bead, map[string]readyLeg, error) {
				return []beads.Bead{{ID: "task", Status: "open"}}, nil, nil
			})
			cleanup, done, err := startHookReadyServerAt(args[i+1], reader)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			select {
			case <-done:
			case <-time.After(testutil.ExecRaceTimeout):
				os.Exit(124)
			}
			os.Exit(0)
		}
		if arg == "--hook-reader" && i+1 < len(args) {
			cmd := newReadyCmd(os.Stdout, os.Stderr)
			cmd.SetArgs(args[1:]) // ready's flags; the hidden path must not open any city.
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			os.Exit(0)
		}
	}
	t.Fatalf("unexpected helper args: %v", args)
}

func startHookReadyServer(reader *hookReadyReader) (string, func(), error) {
	dir, err := os.MkdirTemp("", "gcr-")
	if err != nil {
		return "", nil, err
	}
	endpoint := filepath.Join(dir, "reader.sock")
	stop, _, err := startHookReadyServerAt(endpoint, reader)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	return endpoint, func() { stop(); _ = os.RemoveAll(dir) }, nil
}
