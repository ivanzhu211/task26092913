package tmem

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"sort"
	"time"
)

// Store 是翻译记忆库，持久化在一个本地目录中。
// 所有方法都必须由调用方串行调用（本库不做并发控制）。
type Store struct {
	dir      string
	pairs    map[Pair]*pairIndex
	scratch  searchScratch
	closed   bool
	journal  *os.File
	journalW *bufio.Writer
}

// Open 打开（必要时创建）dir 下的翻译记忆库。
func Open(dir string) (*Store, error) {
	// 本库面向单机大库场景（百万级条目），为把进程内存稳定在 2GB 以内，
	// 在宿主未自行设置内存限制时，将 Go 运行时内存上限设为 1600MB。
	if debug.SetMemoryLimit(-1) == math.MaxInt64 {
		debug.SetMemoryLimit(1600 << 20)
	}
	s := &Store{
		dir:   dir,
		pairs: make(map[Pair]*pairIndex),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Close 将内存状态完整写入目录并关闭。关闭后所有方法返回 ErrClosed。
func (s *Store) Close() error {
	if s.closed {
		return ErrClosed
	}
	if err := s.save(); err != nil {
		return err
	}
	s.closed = true
	return nil
}

// WriteBatch 按批写入一个语言对的条目。
// 任一条目不合法则整批不写入；批内重复（规范化原文相同）以靠后的为准；
// 与库中已有条目重复时以最后一次写入为准（覆盖译文、原文原样和时间）。
// 空批次是合法的空操作。
func (s *Store) WriteBatch(pair Pair, entries []Entry) error {
	if s.closed {
		return ErrClosed
	}
	// 先整批校验，任一不合法则整批拒绝。
	norms := make([]string, len(entries))
	for i, e := range entries {
		norm, err := Normalize(e.Source)
		if err != nil {
			return fmt.Errorf("tmem: entry %d source: %w", i, err)
		}
		if _, err := Normalize(e.Target); err != nil {
			return fmt.Errorf("tmem: entry %d target: %w", i, err)
		}
		norms[i] = norm
	}
	// 先落日志再改内存；日志写失败则整批不生效。
	for _, e := range entries {
		if err := s.appendJournalPut(pair, e.Source, e.Target, e.CompletedAt.UnixNano()); err != nil {
			return fmt.Errorf("tmem: journal: %w", err)
		}
	}
	if err := s.journalW.Flush(); err != nil {
		return fmt.Errorf("tmem: journal: %w", err)
	}
	for i, e := range entries {
		if e.Source == norms[i] {
			s.applyPut(pair, norms[i], norms[i], e.Target, e.CompletedAt.UnixNano())
		} else {
			s.applyPut(pair, norms[i], e.Source, e.Target, e.CompletedAt.UnixNano())
		}
	}
	if ix := s.pairs[pair]; ix.shouldCompact() {
		ix.compact()
	}
	return nil
}

// applyPut 应用单条写入（调用方保证 norm 是 rawSrc 的规范化结果且合法）。
// 供 WriteBatch 与日志回放共用。
func (s *Store) applyPut(pair Pair, norm, rawSrc, target string, at int64) {
	ix := s.pairs[pair]
	if ix == nil {
		ix = newPairIndex()
		s.pairs[pair] = ix
	}
	if old, ok := ix.live[norm]; ok {
		ix.kill(old)
	}
	ix.add(norm, rawSrc, target, at)
}

// Search 在一个语言对内检索与 source 足够相似的条目。
// threshold 为 0 时用 DefaultThreshold（75），合法范围 [75,100]；
// limit 为 0 时用 DefaultLimit（5），合法范围 [1,50]。
// 返回相似度不低于门槛的条目：相似度高的在前；相似度相同，
// 最后写入时间晚的在前；再相同按规范化原文的码点序排。
func (s *Store) Search(pair Pair, source string, threshold, limit int) ([]Hit, error) {
	if s.closed {
		return nil, ErrClosed
	}
	if threshold == 0 {
		threshold = DefaultThreshold
	}
	if threshold < MinThreshold || threshold > MaxThreshold {
		return nil, ErrInvalidThreshold
	}
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit < MinLimit || limit > MaxLimit {
		return nil, ErrInvalidLimit
	}
	norm, err := Normalize(source)
	if err != nil {
		return nil, err
	}
	ix := s.pairs[pair]
	if ix == nil {
		return nil, nil
	}
	q := []rune(norm)
	L := len(q)
	type scored struct {
		id  int32
		sim int
	}
	var found []scored
	for _, id := range ix.candidates(q, threshold, &s.scratch) {
		e := &ix.entries[id]
		er := []rune(e.normSrc)
		m := L
		if len(er) > m {
			m = len(er)
		}
		dmax := m * (100 - threshold) / 100
		d := editDistanceBounded(q, er, dmax)
		if d > dmax {
			continue
		}
		sim := similarity(d, m)
		if sim < threshold {
			continue
		}
		found = append(found, scored{id: id, sim: sim})
	}
	sort.Slice(found, func(i, j int) bool {
		a, b := found[i], found[j]
		if a.sim != b.sim {
			return a.sim > b.sim
		}
		ea, eb := &ix.entries[a.id], &ix.entries[b.id]
		if ea.completedAt != eb.completedAt {
			return ea.completedAt > eb.completedAt
		}
		return ea.normSrc < eb.normSrc
	})
	if len(found) > limit {
		found = found[:limit]
	}
	hits := make([]Hit, len(found))
	for i, f := range found {
		e := &ix.entries[f.id]
		hits[i] = Hit{Source: e.rawSrc, Target: e.target, Similarity: f.sim}
	}
	return hits, nil
}

// Cleanup 删除最后写入时间早于 now 180 天前的条目；正好 180 天的保留。
func (s *Store) Cleanup(now time.Time) error {
	if s.closed {
		return ErrClosed
	}
	s.appendJournalCleanup(now.UnixNano())
	if err := s.journalW.Flush(); err != nil {
		return fmt.Errorf("tmem: journal: %w", err)
	}
	s.applyCleanup(now.UnixNano())
	return nil
}

// applyCleanup 应用清理（now 为 Unix 纳秒）。供 Cleanup 与日志回放共用。
func (s *Store) applyCleanup(now int64) {
	cutoff := now - int64(180*24*time.Hour)
	for _, ix := range s.pairs {
		for _, id := range ix.live {
			if ix.entries[id].completedAt < cutoff {
				ix.kill(id)
			}
		}
		if ix.shouldCompact() {
			ix.compact()
		}
	}
}
