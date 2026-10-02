package main

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

type readyReaderFixture struct {
	calls, closes int
	include       bool
	err           error
}

func (f *readyReaderFixture) Ready(_ context.Context, include bool) ([]beads.Bead, error) {
	f.calls++
	f.include = include
	return []beads.Bead{{ID: string(rune('a' + f.calls))}}, f.err
}
func (f *readyReaderFixture) Close() error { f.closes++; return nil }

func TestControlReadyReadersReuseHandleButReadLive(t *testing.T) {
	fixture := &readyReaderFixture{}
	opens := 0
	readers := &controlReadyReaders{open: func(_ context.Context, root string, env map[string]string) (*controlReadyReader, bool, error) {
		opens++
		if root != "rig" || env["BEADS_DOLT_SERVER_PORT"] != "4567" {
			t.Fatalf("wrong open authority root=%s env=%v", root, env)
		}
		return fixture.reader(), true, nil
	}}
	first, handled, err := readers.ready(t.Context(), "rig", "inputs1", map[string]string{"BEADS_DOLT_SERVER_PORT": "4567"}, true)
	if err != nil || !handled || len(first) != 1 {
		t.Fatalf("first=%v handled=%v err=%v", first, handled, err)
	}
	second, _, err := readers.ready(t.Context(), "rig", "inputs1", map[string]string{"BEADS_DOLT_SERVER_PORT": "4567"}, false)
	if err != nil || len(second) != 1 || first[0].ID == second[0].ID || opens != 1 || fixture.calls != 2 || fixture.include {
		t.Fatalf("stale results or repeated opens: first=%v second=%v opens=%d calls=%d err=%v", first, second, opens, fixture.calls, err)
	}
	if err := readers.close(); err != nil {
		t.Fatal(err)
	}
	if err := readers.close(); err != nil {
		t.Fatal(err)
	}
	if fixture.closes != 1 {
		t.Fatalf("close calls=%d", fixture.closes)
	}
	_, _, err = readers.ready(t.Context(), "rig", "inputs1", nil, false)
	if !errors.Is(err, beads.ErrStoreClosed) {
		t.Fatalf("closed registry returned %v", err)
	}
}

func TestControlReadyReadersInvalidateOpeningInputs(t *testing.T) {
	first, second := &readyReaderFixture{}, &readyReaderFixture{}
	opens := 0
	readers := &controlReadyReaders{open: func(context.Context, string, map[string]string) (*controlReadyReader, bool, error) {
		opens++
		if opens == 1 {
			return first.reader(), true, nil
		}
		return second.reader(), true, nil
	}}
	t.Cleanup(func() { _ = readers.close() })
	_, _, _ = readers.ready(t.Context(), "rig", "metadata+config+credentials1", nil, false)
	_, handled, err := readers.ready(t.Context(), "rig", "metadata+config+credentials2", nil, true)
	if err != nil || !handled || opens != 2 || first.closes != 1 || second.calls != 1 {
		t.Fatalf("invalidated reader not replaced: opens=%d first closes=%d second calls=%d err=%v", opens, first.closes, second.calls, err)
	}
}

func TestControlReadyReadersDoNotMaskLiveFailure(t *testing.T) {
	failure := errors.New("ready read failed")
	readers := &controlReadyReaders{open: func(context.Context, string, map[string]string) (*controlReadyReader, bool, error) {
		return (&readyReaderFixture{err: failure}).reader(), true, nil
	}}
	t.Cleanup(func() { _ = readers.close() })
	rows, handled, err := readers.ready(t.Context(), "rig", "inputs", nil, false)
	if !handled || !errors.Is(err, failure) || rows != nil {
		t.Fatalf("failure masked or partial rows exposed: rows=%v handled=%v err=%v", rows, handled, err)
	}
}

func TestControlReadyReadersIneligibleUsesFallback(t *testing.T) {
	readers := &controlReadyReaders{open: func(context.Context, string, map[string]string) (*controlReadyReader, bool, error) {
		return nil, false, nil
	}}
	rows, handled, err := readers.ready(t.Context(), "rig", "inputs", nil, false)
	if err != nil || handled || rows != nil {
		t.Fatalf("ineligible result=%v handled=%v err=%v", rows, handled, err)
	}
}

func TestControlReadyReadersKeepScopesSeparate(t *testing.T) {
	fixtures := map[string]*readyReaderFixture{
		"/city/first":  {},
		"/city/second": {},
	}
	opens := map[string]int{}
	readers := &controlReadyReaders{open: func(_ context.Context, root string, _ map[string]string) (*controlReadyReader, bool, error) {
		opens[root]++
		return fixtures[root].reader(), true, nil
	}}
	for _, root := range []string{"/city/first", "/city/second", "/city/first"} {
		if _, handled, err := readers.ready(t.Context(), root, "same-inputs", nil, false); err != nil || !handled {
			t.Fatalf("scope %s: handled=%v err=%v", root, handled, err)
		}
	}
	if opens["/city/first"] != 1 || opens["/city/second"] != 1 || fixtures["/city/first"].calls != 2 || fixtures["/city/second"].calls != 1 {
		t.Fatalf("scope handles mixed: opens=%v first=%+v second=%+v", opens, fixtures["/city/first"], fixtures["/city/second"])
	}
	if err := readers.close(); err != nil {
		t.Fatal(err)
	}
	for root, fixture := range fixtures {
		if fixture.closes != 1 {
			t.Fatalf("scope %s close calls=%d", root, fixture.closes)
		}
	}
}

func (f *readyReaderFixture) reader() *controlReadyReader {
	return &controlReadyReader{Ready: f.Ready, Close: f.Close}
}

func TestControlReadyReadersCloseIncompleteHandle(t *testing.T) {
	fixture := &readyReaderFixture{}
	readers := &controlReadyReaders{open: func(context.Context, string, map[string]string) (*controlReadyReader, bool, error) {
		return &controlReadyReader{Close: fixture.Close}, true, nil
	}}
	rows, handled, err := readers.ready(t.Context(), "rig", "inputs", nil, false)
	if err == nil || !handled || rows != nil || fixture.closes != 1 {
		t.Fatalf("incomplete handle leaked: rows=%v handled=%v closes=%d err=%v", rows, handled, fixture.closes, err)
	}
}

func TestControlReadyReadersRejectMissingCloseWithoutPanic(t *testing.T) {
	readers := &controlReadyReaders{open: func(context.Context, string, map[string]string) (*controlReadyReader, bool, error) {
		return &controlReadyReader{}, false, nil
	}}
	rows, handled, err := readers.ready(t.Context(), "rig", "inputs", nil, false)
	if err == nil || !handled || rows != nil {
		t.Fatalf("broken opener accepted: rows=%v handled=%v err=%v", rows, handled, err)
	}
}

func TestControlReadyReadersRememberIneligibilityUntilInputsChange(t *testing.T) {
	opens := 0
	readers := &controlReadyReaders{open: func(context.Context, string, map[string]string) (*controlReadyReader, bool, error) {
		opens++
		return nil, false, nil
	}}
	for _, fingerprint := range []string{"same", "same", "changed"} {
		if _, handled, err := readers.ready(t.Context(), "rig", fingerprint, nil, false); handled || err != nil {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
	}
	if opens != 2 {
		t.Fatalf("ineligible opener repeated %d times", opens)
	}
	if err := readers.close(); err != nil {
		t.Fatal(err)
	}
}

func TestControlReadyReadersBindEnvironmentEvenWhenCallerHashIsUnchanged(t *testing.T) {
	opens := 0
	readers := &controlReadyReaders{open: func(context.Context, string, map[string]string) (*controlReadyReader, bool, error) {
		opens++
		return (&readyReaderFixture{}).reader(), true, nil
	}}
	t.Cleanup(func() { _ = readers.close() })
	for _, port := range []string{"1000", "2000"} {
		if _, _, err := readers.ready(t.Context(), "rig", "same", map[string]string{"BEADS_DOLT_SERVER_PORT": port}, false); err != nil {
			t.Fatal(err)
		}
	}
	if opens != 2 {
		t.Fatal("opening environment detached from hash")
	}
}
