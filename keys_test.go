package main

import (
	"testing"
	"time"
)

func TestKeyPool_PicksHighestCredits(t *testing.T) {
	p := NewKeyPool([]string{"fc-a", "fc-b", "fc-c"})
	p.SetThresholds(10, 2)
	p.SetCredits(0, 50)
	p.SetCredits(1, 200)
	p.SetCredits(2, 30)
	// highest is key 1 (200)
	i, _ := p.Current()
	if i != 1 {
		t.Fatalf("Current() = %d, want 1 (highest credits)", i)
	}
}

func TestKeyPool_FallsBackBelowLowThreshold(t *testing.T) {
	// low=10, stop=2. key0=5 (below low but above stop), key1=1 (below stop).
	p := NewKeyPool([]string{"fc-a", "fc-b"})
	p.SetThresholds(10, 2)
	p.SetCredits(0, 5)
	p.SetCredits(1, 1)
	i, _ := p.Current()
	if i != 0 {
		t.Fatalf("Current() = %d, want 0 (below low but above stop is still usable)", i)
	}
}

func TestKeyPool_NoneUsableBelowStopThreshold(t *testing.T) {
	p := NewKeyPool([]string{"fc-a", "fc-b"})
	p.SetThresholds(10, 2)
	p.SetCredits(0, 1)
	p.SetCredits(1, 0)
	if i, _ := p.Current(); i != -1 {
		t.Fatalf("Current() = %d, want -1 (all below stop threshold)", i)
	}
	if p.AnyUsable() {
		t.Fatal("AnyUsable() = true, want false when all below stop threshold")
	}
}

func TestKeyPool_AdvancePicksDifferentKey(t *testing.T) {
	p := NewKeyPool([]string{"fc-a", "fc-b", "fc-c"})
	p.SetThresholds(10, 2)
	p.SetCredits(0, 100)
	p.SetCredits(1, 50)
	p.SetCredits(2, 200)
	p.Current() // cursor -> 2 (highest). Advance cools down 2.
	i, _ := p.Advance()
	if i == 2 {
		t.Fatalf("Advance() = %d, want a key != 2 (cooled down)", i)
	}
	// next-highest non-cooldown is 0 (100)
	if i != 0 {
		t.Fatalf("Advance() = %d, want 0 (next-highest credits)", i)
	}
}

func TestKeyPool_AdvanceFallsBackToOnlyUsable(t *testing.T) {
	// Only one key above stop threshold; Advance cools it down but with no
	// alternative, the cooldown fallback returns it.
	p := NewKeyPool([]string{"fc-a", "fc-b"})
	p.SetThresholds(10, 2)
	p.SetCredits(0, 100)
	p.SetCredits(1, 0)
	p.Current() // cursor -> 0
	i, _ := p.Advance()
	if i != 0 {
		t.Fatalf("Advance() = %d, want 0 (only usable key, cooldown fallback)", i)
	}
}

func TestKeyPool_RecordSuccessClearsCooldown(t *testing.T) {
	p := NewKeyPool([]string{"fc-a", "fc-b"})
	p.SetThresholds(10, 2)
	p.SetCredits(0, 100)
	p.SetCredits(1, 100)
	p.Current()      // -> 0
	p.Advance()      // cools down 0, next Current -> 1
	p.RecordSuccess(1) // clears 1's cooldown
	// Now both have equal credits and neither in cooldown -> 0 is eligible again
	i, _ := p.Current()
	if i == -1 {
		t.Fatal("Current should pick a key after success cleared cooldown")
	}
}

func TestKeyPool_Decrement(t *testing.T) {
	p := NewKeyPool([]string{"fc-a"})
	p.SetThresholds(10, 2)
	p.SetCredits(0, 100)
	p.Decrement(0, 3)
	if rc := p.snapshotCredits(0); rc != 97 {
		t.Fatalf("after Decrement(3) = %d, want 97", rc)
	}
	// cost <= 0 defaults to 1
	p.Decrement(0, 0)
	if rc := p.snapshotCredits(0); rc != 96 {
		t.Fatalf("after Decrement(0) = %d, want 96 (default cost 1)", rc)
	}
	// never below 0
	p.SetCredits(0, 2)
	p.Decrement(0, 10)
	if rc := p.snapshotCredits(0); rc != 0 {
		t.Fatalf("after over-Decrement = %d, want 0 (clamped)", rc)
	}
}

func TestKeyPool_DecrementUnmeasuredNoOp(t *testing.T) {
	// Unmeasured keys (MaxInt64) must not be estimated down.
	p := NewKeyPool([]string{"fc-a"})
	p.SetThresholds(10, 2)
	p.Decrement(0, 5)
	if rc := p.snapshotCredits(0); rc != -1 {
		t.Fatalf("unmeasured Decrement should stay unmeasured (-1), got %d", rc)
	}
}

func TestKeyPool_DisableSkipsKey(t *testing.T) {
	p := NewKeyPool([]string{"fc-a", "fc-b", "fc-c"})
	p.SetThresholds(10, 2)
	p.SetCredits(0, 100)
	p.SetCredits(1, 50)
	p.SetCredits(2, 200)
	p.Disable(2, time.Now().Add(time.Hour))
	i, _ := p.Current()
	if i == 2 {
		t.Fatalf("Current() returned disabled key 2")
	}
}

func TestKeyPool_DisableZeroesCredits(t *testing.T) {
	p := NewKeyPool([]string{"fc-a"})
	p.SetThresholds(10, 2)
	p.SetCredits(0, 100)
	p.Disable(0, time.Now().Add(time.Hour))
	if rc := p.snapshotCredits(0); rc != 0 {
		t.Fatalf("disabled key credits = %d, want 0", rc)
	}
}

func TestKeyPool_AllDisabledReturnsMinusOne(t *testing.T) {
	p := NewKeyPool([]string{"fc-a", "fc-b"})
	p.SetThresholds(10, 2)
	p.Disable(0, time.Now().Add(time.Hour))
	p.Disable(1, time.Now().Add(time.Hour))
	if i, _ := p.Current(); i != -1 {
		t.Fatalf("Current() with all disabled = %d, want -1", i)
	}
	if i, _ := p.Advance(); i != -1 {
		t.Fatalf("Advance() with all disabled = %d, want -1", i)
	}
	if p.AnyUsable() {
		t.Fatal("AnyUsable() = true, want false when all disabled")
	}
}

func TestKeyPool_ReenableDue(t *testing.T) {
	p := NewKeyPool([]string{"fc-a", "fc-b"})
	p.SetThresholds(10, 2)
	now := time.Now().UTC()
	p.Disable(0, now.Add(-time.Minute))
	p.Disable(1, now.Add(time.Hour))
	if n := p.ReenableDue(now); n != 1 {
		t.Fatalf("ReenableDue re-enabled %d, want 1", n)
	}
	// re-enabled key is unmeasured -> immediately usable
	if i, _ := p.Current(); i == -1 {
		t.Fatal("after re-enabling one key, Current should be usable")
	}
}

func TestKeyPool_StatsAndMasking(t *testing.T) {
	p := NewKeyPool([]string{"fc-abcd1234", "fc-wxyz9876"})
	p.RecordRejection(0, "exhausted")
	p.RecordRejection(0, "exhausted")
	p.RecordRejection(1, "auth")
	p.RecordSuccess(0)

	snap := p.Snapshot()
	if snap.PoolSize != 2 {
		t.Fatalf("PoolSize = %d, want 2", snap.PoolSize)
	}
	if snap.Keys[0].Stats.Exhausted != 2 {
		t.Fatalf("key0 Pay402 = %d, want 2", snap.Keys[0].Stats.Exhausted)
	}
	if snap.Keys[1].Stats.Auth != 1 {
		t.Fatalf("key1 Auth = %d, want 1", snap.Keys[1].Stats.Auth)
	}
	if snap.Keys[0].Stats.Success != 1 {
		t.Fatalf("key0 Success = %d, want 1", snap.Keys[0].Stats.Success)
	}
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

func TestKeyPool_SnapshotShowsCredits(t *testing.T) {
	p := NewKeyPool([]string{"fc-a", "fc-b"})
	p.SetThresholds(10, 2)
	p.SetCredits(0, 42)
	// key1 unmeasured -> snapshot reports -1
	snap := p.Snapshot()
	if snap.Keys[0].RemainingCredits != 42 {
		t.Fatalf("key0 RemainingCredits = %d, want 42", snap.Keys[0].RemainingCredits)
	}
	if snap.Keys[1].RemainingCredits != -1 {
		t.Fatalf("key1 RemainingCredits = %d, want -1 (unmeasured)", snap.Keys[1].RemainingCredits)
	}
}

// snapshotCredits returns a key's remainingCredits as seen by Snapshot (-1 = unmeasured).
func (p *KeyPool) snapshotCredits(index int) int64 {
	snap := p.Snapshot()
	return snap.Keys[index].RemainingCredits
}
