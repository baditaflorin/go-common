package degraded

import (
	"context"
	"testing"
)

func TestSinkTravelsInTheContext(t *testing.T) {
	s := New()
	ctx := NewContext(context.Background(), s)
	FromContext(ctx).Append("ledger-down")
	if got := s.Slice(); len(got) != 1 || got[0] != "ledger-down" {
		t.Fatalf("Slice() = %v, want [ledger-down]", got)
	}
}

func TestNoSinkInTheContextIsSafe(t *testing.T) {
	if s := FromContext(context.Background()); s != nil {
		t.Fatalf("FromContext of a bare context = %v, want nil", s)
	}
	FromContext(context.Background()).Append("x") // a nil Sink ignores appends
}
