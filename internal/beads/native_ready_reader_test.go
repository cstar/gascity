package beads

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

type sdkReadyReaderSpy struct {
	filter beadslib.WorkFilter
	rows   []*beadslib.Issue
	err    error
	closed int
}

func (s *sdkReadyReaderSpy) GetReadyWork(_ context.Context, f beadslib.WorkFilter) ([]*beadslib.Issue, error) {
	s.filter = f
	return s.rows, s.err
}
func (s *sdkReadyReaderSpy) Close() error { s.closed++; return nil }

func TestNativeReadyReaderUsesRawCLIFilterAndStrictConversion(t *testing.T) {
	spy := &sdkReadyReaderSpy{rows: []*beadslib.Issue{{ID: "ready-1", IssueType: beadslib.TypeTask, Status: beadslib.StatusOpen, Metadata: json.RawMessage(`{"gc.routed_to":"rig/control"}`)}}}
	r := &NativeReadyReader{reader: spy}
	rows, err := r.Ready(context.Background(), true, 5000)
	if err != nil || len(rows) != 1 || rows[0].Metadata["gc.routed_to"] != "rig/control" {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	want := beadslib.WorkFilter{Status: beadslib.StatusOpen, SortPolicy: beadslib.SortPolicyPriority, Limit: 5000, IncludeEphemeral: true, ExcludeTypes: []beadslib.IssueType{beadslib.TypeEpic}}
	if !reflect.DeepEqual(spy.filter, want) {
		t.Fatalf("filter=%#v want %#v", spy.filter, want)
	}
	if _, err := r.Ready(context.Background(), false, 1); err != nil || spy.filter.Limit != 1 || spy.filter.IncludeEphemeral {
		t.Fatal("query options were cached across calls")
	}
	failure := errors.New("unavailable")
	spy.err = failure
	if rows, err := r.Ready(context.Background(), false, 5000); rows != nil || !errors.Is(err, failure) {
		t.Fatalf("partial rows=%v err=%v", rows, err)
	}
	spy.err = nil
	spy.rows[0].Metadata = json.RawMessage(`bad`)
	if rows, err := r.Ready(context.Background(), false, 5000); rows != nil || err == nil {
		t.Fatal("invalid metadata accepted")
	}
}

func TestNativeReadyReaderOpeningProjectsAndRestoresSDKNamespace(t *testing.T) {
	t.Setenv("BEADS_CENTRAL_CONFIG", "ambient-central")
	t.Setenv("BEADS_UNSELECTED_FIXTURE", "ambient-other")
	previous := nativeReadyOpen
	defer func() { nativeReadyOpen = previous }()
	spy := &sdkReadyReaderSpy{}
	nativeReadyOpen = func(_ context.Context, dir string) (beadslib.ReadyReader, error) {
		if dir != "/scope/.beads" || os.Getenv("BEADS_CENTRAL_CONFIG") != "selected-central" || os.Getenv("BEADS_UNSELECTED_FIXTURE") != "" || os.Getenv("BEADS_DOLT_AUTO_START") != "0" || os.Getenv("BEADS_DOLT_MAX_CONNS") != "1" {
			t.Fatal("SDK did not receive selected authority")
		}
		return spy, nil
	}
	r, err := OpenNativeReadyReader(context.Background(), "/scope/.beads", map[string]string{"BEADS_CENTRAL_CONFIG": "selected-central"})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("BEADS_CENTRAL_CONFIG") != "ambient-central" || os.Getenv("BEADS_UNSELECTED_FIXTURE") != "ambient-other" {
		t.Fatal("ambient environment not restored")
	}
	if err := r.Close(); err != nil || spy.closed != 1 {
		t.Fatalf("closed=%d err=%v", spy.closed, err)
	}
}

func TestNativeReadyReaderOpeningFailureRestoresAndClosesPartialReader(t *testing.T) {
	t.Setenv("BEADS_CENTRAL_CONFIG", "ambient-central")
	t.Setenv("BEADS_NEW_FIXTURE", "")
	if err := os.Unsetenv("BEADS_NEW_FIXTURE"); err != nil {
		t.Fatal(err)
	}
	previous := nativeReadyOpen
	defer func() { nativeReadyOpen = previous }()
	failure := errors.New("open failure")
	spy := &sdkReadyReaderSpy{}
	nativeReadyOpen = func(_ context.Context, _ string) (beadslib.ReadyReader, error) { return spy, failure }
	r, err := OpenNativeReadyReader(context.Background(), "/scope/.beads", map[string]string{"BEADS_CENTRAL_CONFIG": "selected", "BEADS_NEW_FIXTURE": "temporary"})
	if r != nil || !errors.Is(err, failure) || spy.closed != 1 {
		t.Fatalf("reader=%v err=%v closed=%d", r, err, spy.closed)
	}
	if os.Getenv("BEADS_CENTRAL_CONFIG") != "ambient-central" || os.Getenv("BEADS_NEW_FIXTURE") != "" {
		t.Fatal("SDK failure left scoped environment behind")
	}
}

func TestNativeReadyReaderDeclinesCredentialCommandWithoutInvokingIt(t *testing.T) {
	previous := nativeReadyOpen
	defer func() { nativeReadyOpen = previous }()
	nativeReadyOpen = func(context.Context, string) (beadslib.ReadyReader, error) {
		t.Fatal("unsupported credential helper was invoked")
		return nil, nil
	}
	r, err := OpenNativeReadyReader(context.Background(), "/scope/.beads", map[string]string{"BEADS_DOLT_CREDENTIAL_COMMAND": "fixture credential command"})
	if r != nil || !errors.Is(err, ErrNativeReadyCredentialCommand) {
		t.Fatalf("reader=%v err=%v", r, err)
	}
}
