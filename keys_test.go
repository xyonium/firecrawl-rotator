package main

import (
	"testing"
)

func TestKeyPool_CurrentAndAdvance(t *testing.T) {
	p := NewKeyPool([]string{"fc-a", "fc-b", "fc-c"})
	i, k := p.Current()
	if i != 0 || k != "fc-a" {
		t.Fatalf("Current() = (%d,%q), want (0,fc-a)", i, k)
	}
	i, k = p.Advance()
	if i != 1 || k != "fc-b" {
		t.Fatalf("Advance() = (%d,%q), want (1,fc-b)", i, k)
	}
	i, k = p.Advance()
	if i != 2 || k != "fc-c" {
		t.Fatalf("Advance() = (%d,%q), want (2,fc-c)", i, k)
	}
	// wraps mod N
	i, k = p.Advance()
	if i != 0 || k != "fc-a" {
		t.Fatalf("Advance() wrap = (%d,%q), want (0,fc-a)", i, k)
	}
}

func TestKeyPool_StatsAndMasking(t *testing.T) {
	p := NewKeyPool([]string{"fc-abcd1234", "fc-wxyz9876"})
	p.RecordRejection(0, "402")
	p.RecordRejection(0, "402")
	p.RecordRejection(1, "auth")
	p.RecordSuccess(0)

	snap := p.Snapshot()
	if snap.PoolSize != 2 {
		t.Fatalf("PoolSize = %d, want 2", snap.PoolSize)
	}
	if len(snap.Keys) != 2 {
		t.Fatalf("len(Keys) = %d, want 2", len(snap.Keys))
	}
	if snap.Keys[0].Stats.Pay402 != 2 {
		t.Fatalf("key0 Pay402 = %d, want 2", snap.Keys[0].Stats.Pay402)
	}
	if snap.Keys[1].Stats.Auth != 1 {
		t.Fatalf("key1 Auth = %d, want 1", snap.Keys[1].Stats.Auth)
	}
	if snap.Keys[0].Stats.Success != 1 {
		t.Fatalf("key0 Success = %d, want 1", snap.Keys[0].Stats.Success)
	}
	// masking: last 4 only, prefix hidden
	for _, k := range snap.Keys {
		if k.Last4 != "1234" && k.Last4 != "9876" {
			t.Fatalf("Last4 = %q, want last 4 chars only", k.Last4)
		}
	}
}

func TestKeyPool_RecordRejectionBadKindPanics(t *testing.T) {
	p := NewKeyPool([]string{"fc-a"})
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on bad kind")
		}
	}()
	p.RecordRejection(0, "bogus")
}
