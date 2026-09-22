package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/shellquote"
)

// This private, read-only transport lets the stock shell query reuse one
// dedicated reader's stores. It is scoped to one hook invocation, never inherited as an
// environment variable and never used for claim mutations or custom queries.
// The enclosing 0700 directory limits access to the invoking user.
type hookReadyRequest struct {
	Assignee       string   `json:"assignee"`
	Unassigned     bool     `json:"unassigned"`
	MetadataFields []string `json:"metadata_fields"`
	ExcludeTypes   []string `json:"exclude_types"`
	ExcludeLabels  []string `json:"exclude_labels"`
	SortOrder      string   `json:"sort_order"`
	Limit          int      `json:"limit"`
	Status         string   `json:"status"`
}

type hookReadyResponse struct {
	Rows  []readyBead `json:"rows"`
	Error string      `json:"error,omitempty"`
}

func hookReadyRequestFor(opts readyOpts) hookReadyRequest {
	return hookReadyRequest{Assignee: opts.assignee, Unassigned: opts.unassigned, MetadataFields: opts.metadataFields, ExcludeTypes: opts.excludeTypes, ExcludeLabels: opts.excludeLabels, SortOrder: opts.sortOrder, Limit: opts.limit, Status: opts.status}
}

func (r hookReadyRequest) options() readyOpts {
	return readyOpts{assignee: r.Assignee, unassigned: r.Unassigned, metadataFields: r.MetadataFields, excludeTypes: r.ExcludeTypes, excludeLabels: r.ExcludeLabels, sortOrder: r.SortOrder, limit: r.Limit, status: r.Status}
}

func startHookReadyServerAt(endpoint string, reader *hookReadyReader) (func(), <-chan struct{}, error) {
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		return nil, nil, err
	}
	var once sync.Once
	var mu sync.Mutex
	var active net.Conn
	closed := false
	done := make(chan struct{})
	cleanup := func() {
		once.Do(func() {
			mu.Lock()
			closed = true
			_ = listener.Close()
			if active != nil {
				_ = active.Close()
			}
			mu.Unlock()
		})
	}
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				_ = conn.Close()
				return
			}
			active = conn
			mu.Unlock()
			serveHookReadyConnection(conn, reader)
			_ = conn.Close()
			mu.Lock()
			active = nil
			mu.Unlock()
		}
	}()
	return cleanup, done, nil
}

func serveHookReadyConnection(conn net.Conn, reader *hookReadyReader) {
	if err := conn.SetDeadline(time.Now().Add(hookWorkQueryTimeout)); err != nil {
		return
	}
	var req hookReadyRequest
	var response hookReadyResponse
	if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&req); err != nil {
		response.Error = fmt.Sprintf("decoding hook ready request: %v", err)
	} else if req.Status != "" && req.Status != readyStatusInProgress {
		response.Error = "hook reader only serves ready and in_progress work"
	} else {
		rows, err := reader.query(req.options())
		if err != nil {
			response.Error = err.Error()
		} else {
			response.Rows = rows
		}
	}
	// A disconnected discovery subprocess cannot consume a response; its own
	// deadline/error is authoritative. No mutation is performed by this reader.
	_ = json.NewEncoder(conn).Encode(response)
}

func readHookReadyRemote(endpoint string, opts readyOpts) ([]readyBead, error) {
	conn, err := dialHookReady(endpoint)
	if err != nil {
		return nil, fmt.Errorf("connecting hook reader: %w", err)
	}
	defer conn.Close() //nolint:errcheck // read-only connection
	if err := conn.SetDeadline(time.Now().Add(hookWorkQueryTimeout)); err != nil {
		return nil, err
	}
	if err := json.NewEncoder(conn).Encode(hookReadyRequestFor(opts)); err != nil {
		return nil, err
	}
	var response hookReadyResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return nil, fmt.Errorf("reading hook ready response: %w", err)
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	if response.Rows == nil {
		return nil, errors.New("hook reader returned no result array")
	}
	return response.Rows, nil
}

func hookQueryWithReadyReader(command, endpoint, executable, cityPath string) (string, error) {
	args := shellquote.Split(command)
	if len(args) < 3 || args[0] != "sh" || args[1] != "-c" {
		return "", errors.New("native hook query is not a shell script")
	}
	// This function exists only in the generated discovery shell. Absolute
	// executable binding keeps the private protocol on the current binary even
	// while an operator is qualifying a new build alongside an installed gc.
	cleanup := `kill "$gc_ready_pid" 2>/dev/null; wait "$gc_ready_pid" 2>/dev/null; ` + shellquote.Join([]string{"rm", "-f", endpoint})
	args[2] = shellquote.Join([]string{executable, "ready", "--city", cityPath, "--serve-hook-reader", endpoint}) + ` >/dev/null & gc_ready_pid=$!; trap ` + shellquote.Quote(cleanup) + ` EXIT; gc() { if [ "$1" = ready ]; then shift; ` + shellquote.Join([]string{executable, "ready", "--hook-reader", endpoint}) + ` "$@"; else command gc "$@"; fi; }; ` + args[2]
	return shellquote.Join(args), nil
}

// The reader and first ready client are siblings launched by the same shell.
// Wait only for the listener to appear, never retry a failed query/response.
func dialHookReady(endpoint string) (net.Conn, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", endpoint, time.Second)
		if err == nil {
			return conn, nil
		}
		if !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			return nil, err
		}
		if _, dirErr := os.Stat(filepath.Dir(endpoint)); dirErr != nil {
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
