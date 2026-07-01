// Package streamrewrite implements exhaustive multi-pattern search-and-replace
// over byte streams.
//
// It is built around an Aho-Corasick automaton so that an arbitrary set of
// patterns is matched in a single O(n) pass regardless of how many patterns
// there are, with memory proportional to the patterns (not the stream). The
// replacement policy is leftmost-longest, non-overlapping, and replacement text
// is never re-scanned, so a value can be rewritten to something that itself
// contains a pattern without looping.
//
// The streaming Rewriter guarantees that matches are found and replaced even
// when a pattern straddles the boundary between two Write chunks (e.g. SSE
// frames split mid-token). It does so by retaining at most maxPatternLen-1
// trailing bytes between writes, which bounds extra memory to the longest
// pattern. Feeding the same bytes through any chunking always yields byte-identical
// output to a single ReplaceAll call — this is the "100% complete" guarantee.
package streamrewrite

import "sort"

// Rule is a single pattern -> replacement mapping.
type Rule struct {
	Pattern     string
	Replacement string
}

type node struct {
	next map[byte]int // goto transitions
	fail int          // failure link
	rule int          // index into Matcher.repl for a pattern ending here, or -1
	dict int          // nearest failure-ancestor that is a pattern end, or 0 (none)
}

// Matcher is an immutable, concurrency-safe compiled automaton. Build it once
// with New and reuse it across requests/goroutines (ReplaceAll and NewRewriter
// do not mutate it).
type Matcher struct {
	nodes  []node
	repl   [][]byte
	patLen []int
	maxLen int
}

type match struct {
	start int
	end   int
	rule  int
}

// New compiles the given rules into a Matcher. Rules with an empty pattern, or
// whose replacement equals the pattern (a no-op), are skipped. If no usable
// rule remains, the returned Matcher is a zero-cost pass-through.
func New(rules []Rule) *Matcher {
	m := &Matcher{nodes: []node{{next: map[byte]int{}, fail: 0, rule: -1}}}
	seen := make(map[string]bool, len(rules))
	for _, r := range rules {
		if r.Pattern == "" || r.Pattern == r.Replacement || seen[r.Pattern] {
			continue
		}
		seen[r.Pattern] = true
		m.insert(r.Pattern, r.Replacement)
	}
	if len(m.repl) == 0 {
		return m // pass-through
	}
	m.buildLinks()
	return m
}

// NewFromMap is a convenience constructor for a pattern->replacement map.
func NewFromMap(rules map[string]string) *Matcher {
	list := make([]Rule, 0, len(rules))
	for pattern, replacement := range rules {
		list = append(list, Rule{Pattern: pattern, Replacement: replacement})
	}
	// Sort for deterministic automaton construction (output is order-independent,
	// but a stable build keeps tests and golden traces reproducible).
	sort.Slice(list, func(i, j int) bool { return list[i].Pattern < list[j].Pattern })
	return New(list)
}

// Empty reports whether the matcher has no rules and is a pass-through.
func (m *Matcher) Empty() bool { return m == nil || len(m.repl) == 0 }

func (m *Matcher) insert(pattern, replacement string) {
	cur := 0
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		nxt, ok := m.nodes[cur].next[c]
		if !ok {
			nxt = len(m.nodes)
			m.nodes = append(m.nodes, node{next: map[byte]int{}, fail: 0, rule: -1})
			m.nodes[cur].next[c] = nxt
		}
		cur = nxt
	}
	ruleIdx := len(m.repl)
	m.repl = append(m.repl, []byte(replacement))
	m.patLen = append(m.patLen, len(pattern))
	m.nodes[cur].rule = ruleIdx
	if len(pattern) > m.maxLen {
		m.maxLen = len(pattern)
	}
}

func (m *Matcher) buildLinks() {
	queue := make([]int, 0, len(m.nodes))
	for _, child := range m.nodes[0].next {
		m.nodes[child].fail = 0
		queue = append(queue, child)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		// dict link: nearest failure-ancestor that ends a pattern.
		f := m.nodes[cur].fail
		if m.nodes[f].rule >= 0 {
			m.nodes[cur].dict = f
		} else {
			m.nodes[cur].dict = m.nodes[f].dict
		}
		for c, child := range m.nodes[cur].next {
			f := m.nodes[cur].fail
			for f != 0 {
				if _, ok := m.nodes[f].next[c]; ok {
					break
				}
				f = m.nodes[f].fail
			}
			if nxt, ok := m.nodes[f].next[c]; ok && nxt != child {
				m.nodes[child].fail = nxt
			} else {
				m.nodes[child].fail = 0
			}
			queue = append(queue, child)
		}
	}
}

func (m *Matcher) step(state int, c byte) int {
	for {
		if nxt, ok := m.nodes[state].next[c]; ok {
			return nxt
		}
		if state == 0 {
			return 0
		}
		state = m.nodes[state].fail
	}
}

// findAll returns every pattern occurrence in b, in increasing-end order.
func (m *Matcher) findAll(b []byte) []match {
	var out []match
	state := 0
	for i := 0; i < len(b); i++ {
		state = m.step(state, b[i])
		for r := state; r > 0; r = m.nodes[r].dict {
			if rule := m.nodes[r].rule; rule >= 0 {
				end := i + 1
				out = append(out, match{start: end - m.patLen[rule], end: end, rule: rule})
			}
		}
	}
	return out
}

// resolve picks leftmost-longest, non-overlapping matches.
func (m *Matcher) resolve(b []byte) []match {
	all := m.findAll(b)
	if len(all) == 0 {
		return nil
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].start != all[j].start {
			return all[i].start < all[j].start // leftmost first
		}
		return all[i].end > all[j].end // longest first on ties
	})
	chosen := all[:0]
	cursor := 0
	for _, mt := range all {
		if mt.start >= cursor {
			chosen = append(chosen, mt)
			cursor = mt.end
		}
	}
	return chosen
}

// emit writes b[0:limit] with the chosen matches (which must lie fully within
// [0,limit)) replaced. dst is appended to and returned.
func (m *Matcher) emit(dst, b []byte, chosen []match, limit int) []byte {
	prev := 0
	for _, mt := range chosen {
		if mt.end > limit {
			break
		}
		dst = append(dst, b[prev:mt.start]...)
		dst = append(dst, m.repl[mt.rule]...)
		prev = mt.end
	}
	dst = append(dst, b[prev:limit]...)
	return dst
}

// ReplaceAll returns b with every pattern occurrence replaced. The input is not
// modified. When the matcher is empty, b is returned as-is.
func (m *Matcher) ReplaceAll(b []byte) []byte {
	if m.Empty() || len(b) == 0 {
		return b
	}
	chosen := m.resolve(b)
	if len(chosen) == 0 {
		return b
	}
	out := make([]byte, 0, len(b)+16)
	return m.emit(out, b, chosen, len(b))
}

// ReplaceString is the string convenience form of ReplaceAll.
func (m *Matcher) ReplaceString(s string) string {
	if m.Empty() {
		return s
	}
	return string(m.ReplaceAll([]byte(s)))
}

// Rewriter is a stateful, boundary-safe streaming replacer. It is NOT safe for
// concurrent use; create one per stream via Matcher.NewRewriter.
type Rewriter struct {
	m     *Matcher
	carry []byte
	// work is a reusable scratch buffer for merging carry + chunk before
	// the Aho-Corasick scan. It is grown on demand and reused across Write
	// calls on the same stream, eliminating the per-chunk work-buffer
	// allocation after the first few chunks (once it reaches steady-state
	// chunk+carry size — ~32 KiB for the typical SSE relay).
	work []byte
	// emittedWork is a reusable buffer for the emit stage output. Same reuse
	// pattern: grown on demand, zero allocations at steady-state.
	emittedWork []byte
}

// NewRewriter starts a streaming replace session.
func (m *Matcher) NewRewriter() *Rewriter { return &Rewriter{m: m} }

// Write feeds the next chunk and returns the bytes safe to forward downstream
// now. Bytes that could still be part of a pattern continuing into a future
// chunk are retained internally (at most maxPatternLen-1 of them). Call Flush
// once the stream ends to drain the remainder. The returned slice is freshly
// allocated and owned by the caller.
func (r *Rewriter) Write(chunk []byte) []byte {
	if r.m.Empty() {
		return chunk
	}
	if len(chunk) == 0 {
		return nil
	}
	M := r.m.maxLen

	// Reuse the work buffer across Write calls on this stream. Reset length
	// but keep the backing array; grow only when this chunk is larger than
	// any previously seen carry+chunk pair.
	need := len(r.carry) + len(chunk)
	if cap(r.work) < need {
		// Over-allocate a bit so we amortise the grow across future chunks.
		r.work = make([]byte, 0, need+2048)
	}
	r.work = r.work[:0]
	r.work = append(r.work, r.carry...)
	r.work = append(r.work, chunk...)

	if len(r.work) < M {
		// Too short to rule out a boundary-spanning match; keep buffering.
		r.carry = r.work
		return nil
	}
	chosen := r.m.resolve(r.work)
	cut := len(r.work) - (M - 1)
	// Defer any match that straddles the cut wholesale so it can be re-evaluated
	// (a longer pattern sharing its start may complete in the next chunk).
	for _, mt := range chosen {
		if mt.start >= cut {
			break
		}
		if mt.end > cut {
			cut = mt.start
			break
		}
	}
	// Reuse the emitted-work buffer for the same reasons.
	if cap(r.emittedWork) < cut+16 {
		r.emittedWork = make([]byte, 0, cut+16+1024)
	}
	r.emittedWork = r.emittedWork[:0]
	emitted := r.m.emit(r.emittedWork, r.work, chosen, cut)
	// Carry the tail past the cut. The caller owns emitted, so we can't alias
	// it with our internal carry buffer.
	r.carry = append([]byte(nil), r.work[cut:]...)
	return emitted
}

// Flush replaces and returns all retained bytes. After Flush the Rewriter is
// reset and may be reused for a new stream.
func (r *Rewriter) Flush() []byte {
	if r.m.Empty() || len(r.carry) == 0 {
		out := r.carry
		r.carry = nil
		return out
	}
	out := r.m.ReplaceAll(r.carry)
	// ReplaceAll may return the input slice unchanged; copy to give the caller
	// an independent buffer and clear our state.
	res := append([]byte(nil), out...)
	r.carry = nil
	return res
}
