package api

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

const codexThreadHandleTTL = 15 * time.Minute

// CodexRuntimeDescriptor is the in-memory ownership record. The owner fields
// are deliberately excluded from JSON: clients receive only safe availability
// metadata after owner filtering.
type CodexRuntimeDescriptor struct {
	ID             string    `json:"id"`
	Label          string    `json:"label,omitempty"`
	OwnerPrincipal string    `json:"-"`
	OwnerGroup     string    `json:"-"`
	Generation     uint64    `json:"generation"`
	Available      bool      `json:"available"`
	LastHeartbeat  time.Time `json:"last_heartbeat,omitempty"`
}

type codexRuntimeEntry struct {
	CodexRuntimeDescriptor
	Runtime CodexThreadRuntime

	mu       sync.RWMutex
	statuses map[string]ThreadStatusChanged
}

// CodexRuntimeRegistry stores availability and encrypted locator handles, not
// transcripts, prompts, outputs, or raw IDs in any response/audit record.
type CodexRuntimeRegistry struct {
	mu       sync.RWMutex
	runtimes map[string]*codexRuntimeEntry
	key      []byte
}

func NewCodexRuntimeRegistry(secret []byte) *CodexRuntimeRegistry {
	sum := sha256.Sum256(secret)
	return &CodexRuntimeRegistry{
		runtimes: make(map[string]*codexRuntimeEntry),
		key:      append([]byte(nil), sum[:]...),
	}
}

func (r *CodexRuntimeRegistry) Register(id string, runtime CodexThreadRuntime, descriptor CodexRuntimeDescriptor) error {
	if r == nil {
		return errors.New("codex runtime registry unavailable")
	}
	id = strings.TrimSpace(id)
	if id == "" || runtime == nil {
		return errors.New("runtime id and adapter are required")
	}
	if descriptor.Generation == 0 {
		descriptor.Generation = 1
	}
	descriptor.ID = id
	descriptor.Available = true
	if descriptor.LastHeartbeat.IsZero() {
		descriptor.LastHeartbeat = time.Now().UTC()
	}
	r.mu.Lock()
	if previous := r.runtimes[id]; previous != nil {
		previous.mu.RLock()
		previousGeneration := previous.Generation
		previous.mu.RUnlock()
		// A replacement adapter invalidates every browser handle issued by the
		// prior runtime generation, even when its caller omitted a generation.
		if descriptor.Generation <= previousGeneration {
			descriptor.Generation = previousGeneration + 1
		}
	}
	r.runtimes[id] = &codexRuntimeEntry{
		CodexRuntimeDescriptor: descriptor,
		Runtime:                runtime,
		statuses:               make(map[string]ThreadStatusChanged),
	}
	r.mu.Unlock()
	return nil
}

func (r *CodexRuntimeRegistry) SetAvailable(id string, available bool) {
	entry, ok := r.get(id)
	if !ok {
		return
	}
	entry.mu.Lock()
	entry.Available = available
	entry.LastHeartbeat = time.Now().UTC()
	entry.mu.Unlock()
}

func (r *CodexRuntimeRegistry) get(id string) (*codexRuntimeEntry, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	entry, ok := r.runtimes[strings.TrimSpace(id)]
	r.mu.RUnlock()
	return entry, ok
}

type codexRuntimePrincipal struct {
	ID    string
	Group string
}

func (entry *codexRuntimeEntry) allows(principal codexRuntimePrincipal) bool {
	if entry == nil {
		return false
	}
	entry.mu.RLock()
	owner, group := entry.OwnerPrincipal, entry.OwnerGroup
	entry.mu.RUnlock()
	if owner == "" && group == "" {
		return true
	}
	return (owner != "" && owner == principal.ID) || (group != "" && group == principal.Group)
}

func (entry *codexRuntimeEntry) available() bool {
	if entry == nil {
		return false
	}
	entry.mu.RLock()
	available := entry.Available
	entry.mu.RUnlock()
	return available
}

func (r *CodexRuntimeRegistry) ListFor(principal codexRuntimePrincipal) []CodexRuntimeDescriptor {
	if r == nil {
		return []CodexRuntimeDescriptor{}
	}
	r.mu.RLock()
	entries := make([]*codexRuntimeEntry, 0, len(r.runtimes))
	for _, entry := range r.runtimes {
		entries = append(entries, entry)
	}
	r.mu.RUnlock()
	result := make([]CodexRuntimeDescriptor, 0, len(entries))
	for _, entry := range entries {
		if !entry.allows(principal) {
			continue
		}
		entry.mu.RLock()
		result = append(result, CodexRuntimeDescriptor{
			ID:            entry.ID,
			Label:         entry.Label,
			Generation:    entry.Generation,
			Available:     entry.Available,
			LastHeartbeat: entry.LastHeartbeat,
		})
		entry.mu.RUnlock()
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// codexHandle is AES-GCM sealed before being returned. It may contain a raw
// app-server locator, but never appears plaintext in browser-visible JSON.
type codexHandle struct {
	RuntimeID  string `json:"r"`
	ThreadID   string `json:"t,omitempty"`
	TurnID     string `json:"u,omitempty"`
	Principal  string `json:"p"`
	Group      string `json:"g,omitempty"`
	Generation uint64 `json:"n"`
	ExpiresAt  int64  `json:"e"`
	Kind       string `json:"k"`
	Cursor     string `json:"c,omitempty"`
	FilterHash string `json:"f,omitempty"`
}

func (r *CodexRuntimeRegistry) seal(handle codexHandle) (string, error) {
	if r == nil || len(r.key) != 32 {
		return "", errors.New("codex runtime handle key unavailable")
	}
	plain, err := json.Marshal(handle)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(r.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nil, nonce, plain, []byte("codex-thread-handle-v1"))
	return base64.RawURLEncoding.EncodeToString(append(nonce, ciphertext...)), nil
}

func (r *CodexRuntimeRegistry) open(value, kind string, principal codexRuntimePrincipal) (codexHandle, error) {
	if r == nil || len(r.key) != 32 {
		return codexHandle{}, errors.New("codex runtime handle key unavailable")
	}
	encoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return codexHandle{}, errors.New("invalid opaque handle")
	}
	block, err := aes.NewCipher(r.key)
	if err != nil {
		return codexHandle{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(encoded) < gcm.NonceSize() {
		return codexHandle{}, errors.New("invalid opaque handle")
	}
	plain, err := gcm.Open(nil, encoded[:gcm.NonceSize()], encoded[gcm.NonceSize():], []byte("codex-thread-handle-v1"))
	if err != nil {
		return codexHandle{}, errors.New("invalid opaque handle")
	}
	var handle codexHandle
	if err := json.Unmarshal(plain, &handle); err != nil || handle.Kind != kind || handle.ExpiresAt < time.Now().Unix() {
		return codexHandle{}, errors.New("expired or invalid opaque handle")
	}
	if handle.Principal != principal.ID || handle.Group != principal.Group {
		return codexHandle{}, errors.New("opaque handle owner mismatch")
	}
	return handle, nil
}

func (r *CodexRuntimeRegistry) ThreadHandle(principal codexRuntimePrincipal, entry *codexRuntimeEntry, thread Thread) (string, error) {
	if entry == nil || strings.TrimSpace(thread.ID) == "" {
		return "", errors.New("thread identity unavailable")
	}
	entry.mu.RLock()
	runtimeID, generation := entry.ID, entry.Generation
	entry.mu.RUnlock()
	return r.seal(codexHandle{
		RuntimeID: runtimeID, ThreadID: thread.ID, Principal: principal.ID, Group: principal.Group,
		Generation: generation, ExpiresAt: time.Now().Add(codexThreadHandleTTL).Unix(), Kind: "thread",
	})
}

// ThreadKey is a stable, non-authorizing correlation key for a browser list.
// ThreadHandle deliberately uses fresh AEAD randomness on every projection, so
// it cannot be used to correlate a later status event with an existing row.
// This keyed digest solves that UI-only correlation problem without revealing a
// raw app-server thread locator or becoming an accepted capability.
func (r *CodexRuntimeRegistry) ThreadKey(principal codexRuntimePrincipal, entry *codexRuntimeEntry, thread Thread) (string, error) {
	if r == nil || len(r.key) != 32 || entry == nil || strings.TrimSpace(thread.ID) == "" {
		return "", errors.New("thread identity unavailable")
	}
	entry.mu.RLock()
	runtimeID, generation := entry.ID, entry.Generation
	entry.mu.RUnlock()
	mac := hmac.New(sha256.New, r.key)
	for _, value := range []string{"codex-thread-status-key/v1", principal.ID, principal.Group, runtimeID, fmt.Sprintf("%d", generation), thread.ID} {
		_, _ = mac.Write([]byte(value))
		_, _ = mac.Write([]byte{0})
	}
	// 128 bits is ample for a short-lived UI correlation key and keeps it clearly
	// distinct from the longer opaque capability handle shown in the table.
	return hex.EncodeToString(mac.Sum(nil)[:16]), nil
}

func (r *CodexRuntimeRegistry) TurnHandle(principal codexRuntimePrincipal, entry *codexRuntimeEntry, thread Thread) (string, error) {
	if entry == nil || strings.TrimSpace(thread.ID) == "" || strings.TrimSpace(thread.ActiveTurnID) == "" {
		return "", nil
	}
	entry.mu.RLock()
	runtimeID, generation := entry.ID, entry.Generation
	entry.mu.RUnlock()
	return r.seal(codexHandle{
		RuntimeID: runtimeID, ThreadID: thread.ID, TurnID: thread.ActiveTurnID, Principal: principal.ID, Group: principal.Group,
		Generation: generation, ExpiresAt: time.Now().Add(codexThreadHandleTTL).Unix(), Kind: "turn",
	})
}

func (r *CodexRuntimeRegistry) Cursor(principal codexRuntimePrincipal, runtimeID, cursor, filterHash string, generation uint64) (string, error) {
	if strings.TrimSpace(cursor) == "" {
		return "", nil
	}
	return r.seal(codexHandle{
		RuntimeID: runtimeID, Principal: principal.ID, Group: principal.Group, Generation: generation,
		ExpiresAt: time.Now().Add(codexThreadHandleTTL).Unix(), Kind: "cursor", Cursor: cursor, FilterHash: filterHash,
	})
}

func (r *CodexRuntimeRegistry) updateStatus(runtimeID string, status ThreadStatusChanged) bool {
	entry, ok := r.get(runtimeID)
	if !ok || strings.TrimSpace(status.ThreadID) == "" {
		return false
	}
	entry.mu.Lock()
	previous, exists := entry.statuses[status.ThreadID]
	if exists && status.Revision <= previous.Revision {
		entry.mu.Unlock()
		return false
	}
	entry.statuses[status.ThreadID] = status
	entry.LastHeartbeat = time.Now().UTC()
	entry.mu.Unlock()
	return true
}

func (r *CodexRuntimeRegistry) Subscribe(ctx context.Context, runtimeID string) (<-chan ThreadStatusChanged, error) {
	entry, ok := r.get(runtimeID)
	if !ok || !entry.available() {
		return nil, errors.New("codex runtime unavailable")
	}
	return entry.Runtime.SubscribeThreadStatus(ctx, runtimeID)
}
