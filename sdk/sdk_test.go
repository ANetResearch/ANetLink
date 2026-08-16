package sdk

import (
	"context"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
)

type stubBackend struct {
	name string
	ok   bool
}

func (s stubBackend) Name() string { return s.name }
func (s stubBackend) Probe(context.Context) Availability {
	return Availability{Available: s.ok, Detail: "stub"}
}

func TestSelectFirstAvailable(t *testing.T) {
	b, err := Select(context.Background(), "", stubBackend{"host", false}, stubBackend{"native", true})
	if err != nil || b.Name() != "native" {
		t.Fatalf("want native fallback, got %v %v", b, err)
	}
}

func TestSelectPrefersPriorityOrder(t *testing.T) {
	b, _ := Select(context.Background(), "", stubBackend{"host", true}, stubBackend{"native", true})
	if b.Name() != "host" {
		t.Fatal("host facility must win when available (A5 探测优先)")
	}
}

func TestSelectOverride(t *testing.T) {
	b, err := Select(context.Background(), "native", stubBackend{"host", true}, stubBackend{"native", true})
	if err != nil || b.Name() != "native" {
		t.Fatalf("override ignored: %v %v", b, err)
	}
	if _, err := Select(context.Background(), "native", stubBackend{"host", true}, stubBackend{"native", false}); err == nil {
		t.Fatal("forced-but-dead backend must be a configuration error")
	}
	if _, err := Select(context.Background(), "ghost", stubBackend{"host", true}); err == nil {
		t.Fatal("unknown override must error")
	}
}

func TestSelectNoneAvailable(t *testing.T) {
	_, err := Select(context.Background(), "", stubBackend{"a", false}, stubBackend{"b", false})
	if err == nil || !strings.Contains(err.Error(), "a: stub") {
		t.Fatalf("error must carry per-backend detail, got %v", err)
	}
}

func TestEffectBuilderHonesty(t *testing.T) {
	e := NewEffect().Build()
	if e.Status != effect.Unverified {
		t.Fatal("default must be UNVERIFIED")
	}
	e = NewEffect().Verified().Build()
	if e.Status != effect.Unverified {
		t.Fatal("OK without metrics must degrade to UNVERIFIED")
	}
	e = NewEffect().Metric("power_state", 1).Verified().Build()
	if !e.Verifiable() {
		t.Fatal("metric + Verified must be verifiable")
	}
	e = NewEffect().Fail("boom").Build()
	if e.Status != effect.Failed || e.Message != "boom" {
		t.Fatal("Fail lost information")
	}
}
