package tmem

import (
	"sort"
	"testing"
	"time"
)

// bruteModel 是按业务规则逐条暴力比对的参考实现，用于对照验证。
type bruteModel struct {
	entries map[Pair]map[string]bruteEntry // pair -> 规范化原文 -> 条目
}

type bruteEntry struct {
	raw    string
	target string
	at     int64
}

func newBruteModel() *bruteModel {
	return &bruteModel{entries: make(map[Pair]map[string]bruteEntry)}
}

func (m *bruteModel) write(pair Pair, entries []Entry) {
	norms := make([]string, len(entries))
	for i, e := range entries {
		n, err := Normalize(e.Source)
		if err != nil {
			panic("test: invalid source")
		}
		if _, err := Normalize(e.Target); err != nil {
			panic("test: invalid target")
		}
		norms[i] = n
	}
	em := m.entries[pair]
	if em == nil {
		em = make(map[string]bruteEntry)
		m.entries[pair] = em
	}
	for i, e := range entries {
		em[norms[i]] = bruteEntry{raw: e.Source, target: e.Target, at: e.CompletedAt.UnixNano()}
	}
}

func (m *bruteModel) cleanup(now time.Time) {
	cutoff := now.Add(-180 * 24 * time.Hour).UnixNano()
	for _, em := range m.entries {
		for norm, e := range em {
			if e.at < cutoff {
				delete(em, norm)
			}
		}
	}
}

func (m *bruteModel) search(pair Pair, source string, threshold, limit int) []Hit {
	if threshold == 0 {
		threshold = DefaultThreshold
	}
	if limit == 0 {
		limit = DefaultLimit
	}
	norm, err := Normalize(source)
	if err != nil {
		return nil
	}
	q := []rune(norm)
	em := m.entries[pair]
	type scored struct {
		norm string
		e    bruteEntry
		sim  int
	}
	var found []scored
	for ns, e := range em {
		er := []rune(ns)
		m := len(q)
		if len(er) > m {
			m = len(er)
		}
		d := editDistanceBounded(q, er, -1)
		sim := similarity(d, m)
		if sim >= threshold {
			found = append(found, scored{norm: ns, e: e, sim: sim})
		}
	}
	sort.Slice(found, func(i, j int) bool {
		a, b := found[i], found[j]
		if a.sim != b.sim {
			return a.sim > b.sim
		}
		if a.e.at != b.e.at {
			return a.e.at > b.e.at
		}
		return a.norm < b.norm
	})
	if len(found) > limit {
		found = found[:limit]
	}
	hits := make([]Hit, len(found))
	for i, f := range found {
		hits[i] = Hit{Source: f.e.raw, Target: f.e.target, Similarity: f.sim}
	}
	return hits
}

func equalHits(a, b []Hit) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func checkSearch(t *testing.T, s *Store, m *bruteModel, pair Pair, q string, th, lim int) {
	t.Helper()
	got, err := s.Search(pair, q, th, lim)
	if err != nil {
		t.Fatalf("Search(%q, th=%d, lim=%d) error: %v", q, th, lim, err)
	}
	want := m.search(pair, q, th, lim)
	if !equalHits(got, want) {
		t.Fatalf("Search(%q, th=%d, lim=%d) mismatch:\n got: %+v\nwant: %+v", q, th, lim, got, want)
	}
}
