package reliability

import (
	"testing"
	"time"
)

func TestWorkingStateMergeFacts(t *testing.T) {
	var ws WorkingState
	f := Facts{
		FirstUserText:   "build the cache layer",
		Commands:        []string{"go test ./..."},
		FilesSeen:       []string{"cache.go"},
		Tools:           []string{"shell"},
		HasTestEvidence: true,
	}
	ws.MergeFacts(f, Classification{Task: TaskCoding, Risk: RiskHigh})
	if ws.Objective != "build the cache layer" {
		t.Errorf("Objective = %q", ws.Objective)
	}
	if ws.TaskType != "coding" || ws.RiskLevel != "high" {
		t.Errorf("task/risk = %q/%q", ws.TaskType, ws.RiskLevel)
	}
	if !contains(ws.CommandsRun, "go test ./...") {
		t.Errorf("CommandsRun = %v", ws.CommandsRun)
	}
	if !contains(ws.FilesSeen, "cache.go") {
		t.Errorf("FilesSeen = %v", ws.FilesSeen)
	}
	if ws.VerificationStatus != "tests_run" {
		t.Errorf("VerificationStatus = %q, want tests_run", ws.VerificationStatus)
	}
}

func TestWorkingStateObjectiveStable(t *testing.T) {
	var ws WorkingState
	ws.MergeFacts(Facts{FirstUserText: "first goal"}, Classification{Task: TaskCoding})
	ws.MergeFacts(Facts{FirstUserText: "second goal"}, Classification{Task: TaskCoding})
	if ws.Objective != "first goal" {
		t.Fatalf("Objective should stay the first goal, got %q", ws.Objective)
	}
}

func TestWorkingStateListBounded(t *testing.T) {
	var ws WorkingState
	for i := 0; i < maxStateList*3; i++ {
		ws.MergeFacts(Facts{Commands: []string{string(rune('a'+i%26)) + itoa(i)}}, Classification{})
	}
	if len(ws.CommandsRun) > maxStateList {
		t.Fatalf("CommandsRun grew unbounded: %d > %d", len(ws.CommandsRun), maxStateList)
	}
}

func TestStoreUpdateGet(t *testing.T) {
	s := NewStore(time.Minute, 100)
	s.Update("k1", func(ws *WorkingState) { ws.Objective = "obj" })
	got, ok := s.Get("k1")
	if !ok || got.Objective != "obj" {
		t.Fatalf("Get after Update = %+v ok=%v", got, ok)
	}
	// Returned value is a copy: mutating it must not change the store.
	got.Objective = "mutated"
	again, _ := s.Get("k1")
	if again.Objective != "obj" {
		t.Fatalf("store value was mutated through returned copy: %q", again.Objective)
	}
}

func TestStoreEmptyKeyNoop(t *testing.T) {
	s := NewStore(time.Minute, 100)
	s.Update("", func(ws *WorkingState) { ws.Objective = "x" })
	if _, ok := s.Get(""); ok {
		t.Fatal("empty key should never be stored")
	}
}

func TestStoreExpiry(t *testing.T) {
	s := NewStore(time.Millisecond, 100)
	s.Update("k", func(ws *WorkingState) { ws.Objective = "o" })
	time.Sleep(5 * time.Millisecond)
	if _, ok := s.Get("k"); ok {
		t.Fatal("entry should have expired")
	}
}

func TestStoreEviction(t *testing.T) {
	s := NewStore(time.Hour, 3)
	for i := 0; i < 10; i++ {
		s.Update("k"+itoa(i), func(ws *WorkingState) { ws.Objective = itoa(i) })
		time.Sleep(time.Millisecond) // ensure distinct updated timestamps for LRU
	}
	s.mu.Lock()
	n := len(s.m)
	s.mu.Unlock()
	if n > 3 {
		t.Fatalf("store exceeded max size: %d > 3", n)
	}
}

func TestNilStoreSafe(t *testing.T) {
	var s *Store
	if _, ok := s.Get("k"); ok {
		t.Fatal("nil store Get should be false")
	}
	ws := s.Update("k", func(ws *WorkingState) { ws.Objective = "x" })
	if ws.Objective != "x" {
		t.Fatalf("nil store Update should still run fn on a throwaway value: %q", ws.Objective)
	}
}

// itoa avoids importing strconv just for tests.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
