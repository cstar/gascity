package main

import (
	"context"
	"errors"
	"testing"
)

func TestControlReadyOpenVerifiedRejectsChangedInputs(t *testing.T) {
	f := &readyReaderFixture{}
	before := &controlReadyOpeningSnapshot{Fingerprint: "first"}
	reader, eligible, err := controlReadyOpenVerified(t.Context(), before,
		func(context.Context, map[string]string) (*controlReadyReader, bool, error) {
			return f.reader(), true, nil
		},
		func() (*controlReadyOpeningSnapshot, error) {
			return &controlReadyOpeningSnapshot{Fingerprint: "changed"}, nil
		})
	if err == nil || eligible || reader != nil || f.closes != 1 || f.calls != 0 {
		t.Fatalf("changed input reader retained: eligible=%v closes=%d reads=%d err=%v", eligible, f.closes, f.calls, err)
	}
}

func TestControlReadyOpenVerifiedClosesOnRecaptureFailure(t *testing.T) {
	f := &readyReaderFixture{}
	failure := errors.New("input unreadable")
	reader, _, err := controlReadyOpenVerified(t.Context(), &controlReadyOpeningSnapshot{Fingerprint: "first"},
		func(context.Context, map[string]string) (*controlReadyReader, bool, error) {
			return f.reader(), true, nil
		},
		func() (*controlReadyOpeningSnapshot, error) { return nil, failure })
	if reader != nil || !errors.Is(err, failure) || f.closes != 1 {
		t.Fatalf("reader leaked after recapture failure: closes=%d err=%v", f.closes, err)
	}
}

func TestControlReadyOpenVerifiedKeepsStableReaderAndSkipsIneligible(t *testing.T) {
	for _, eligible := range []bool{true, false} {
		f := &readyReaderFixture{}
		captures := 0
		before := &controlReadyOpeningSnapshot{Fingerprint: "first"}
		reader, handled, err := controlReadyOpenVerified(t.Context(), before,
			func(context.Context, map[string]string) (*controlReadyReader, bool, error) {
				if !eligible {
					return nil, false, nil
				}
				return f.reader(), true, nil
			},
			func() (*controlReadyOpeningSnapshot, error) { captures++; return before, nil })
		if err != nil || handled != eligible || f.closes != 0 {
			t.Fatalf("eligible=%v handled=%v closes=%d err=%v", eligible, handled, f.closes, err)
		}
		if eligible {
			if reader == nil || captures != 1 {
				t.Fatal("stable reader lost")
			}
			_ = reader.Close()
		} else if captures != 0 {
			t.Fatal("ineligible opening rechecked")
		}
	}
}
