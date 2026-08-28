package cache

import (
	"testing"
	"time"

	"huawei-cpe/internal/poller"
)

func TestPutGet(t *testing.T) {
	s := New()
	now := time.Now()
	snap := poller.Snapshot{At: now, Online: true}
	snap.Signal.RSRP = -80

	s.PutSnapshot("main", snap)

	got, fresh, ok := s.Get("main")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if !fresh {
		t.Error("expected fresh snapshot")
	}
	if !got.Online || got.Signal.RSRP != -80 {
		t.Errorf("snapshot mismatch: %+v", got)
	}
}

func TestGetMiss(t *testing.T) {
	s := New()
	_, _, ok := s.Get("nope")
	if ok {
		t.Error("expected miss for unknown device")
	}
}

func TestStale(t *testing.T) {
	s := New()
	s.PutSnapshot("main", poller.Snapshot{})
	// 直接回拨写入时间模拟陈旧
	s.mu.Lock()
	it := s.items["main"]
	it.at = time.Now().Add(-StaleTTL - time.Second)
	s.items["main"] = it
	s.mu.Unlock()

	_, fresh, ok := s.Get("main")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if fresh {
		t.Error("expected stale snapshot")
	}
}

func TestIDsAndRemove(t *testing.T) {
	s := New()
	s.PutSnapshot("a", poller.Snapshot{})
	s.PutSnapshot("b", poller.Snapshot{})

	ids := s.IDs()
	if len(ids) != 2 {
		t.Fatalf("IDs = %v, want 2 entries", ids)
	}

	s.Remove("a")
	if _, _, ok := s.Get("a"); ok {
		t.Error("expected miss after Remove")
	}
	if _, _, ok := s.Get("b"); !ok {
		t.Error("expected hit for remaining device")
	}
}
