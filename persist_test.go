package tmem

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// TestReopen 写入（含覆盖与清理）后关闭重开，检索结果必须与暴力模型一致。
func TestReopen(t *testing.T) {
	dir := t.TempDir()
	m := newBruteModel()
	rng := rand.New(rand.NewSource(11))
	pairs := []Pair{testPair, {Source: "zh", Target: "en"}}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 20; round++ {
		pair := pairs[rng.Intn(len(pairs))]
		n := 1 + rng.Intn(10)
		batch := make([]Entry, n)
		for i := range batch {
			batch[i] = Entry{
				Source:      fmt.Sprintf("sentence %d number %d 条约", rng.Intn(40), rng.Intn(5)),
				Target:      fmt.Sprintf("T-%d-%d", round, i),
				CompletedAt: at(rng.Intn(300)),
			}
		}
		if err := s.WriteBatch(pair, batch); err != nil {
			t.Fatal(err)
		}
		m.write(pair, batch)
	}
	if err := s.Cleanup(at(200)); err != nil {
		t.Fatal(err)
	}
	m.cleanup(at(200))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 快照存在、日志已清除
	if _, err := os.Stat(filepath.Join(dir, snapshotFile)); err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, journalFile)); !os.IsNotExist(err) {
		t.Fatalf("journal should be removed after Close, stat err=%v", err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	for _, pair := range pairs {
		for _, q := range []string{"sentence 1 number 1 条约", "sentence 40 number 4 条约", "sentence 5 number 2 条约!"} {
			for _, th := range []int{75, 90, 100} {
				checkSearch(t, s2, m, pair, q, th, 0)
				checkSearch(t, s2, m, pair, q, th, 50)
			}
		}
	}
	// 重开后继续写入、清理，结果仍应一致
	extra := []Entry{{Source: "sentence 1 number 1 条约", Target: "new", CompletedAt: at(250)}}
	if err := s2.WriteBatch(pairs[0], extra); err != nil {
		t.Fatal(err)
	}
	m.write(pairs[0], extra)
	if err := s2.Cleanup(at(400)); err != nil {
		t.Fatal(err)
	}
	m.cleanup(at(400))
	for _, pair := range pairs {
		for _, q := range []string{"sentence 1 number 1 条约", "sentence 2 number 2 条约"} {
			checkSearch(t, s2, m, pair, q, 75, 0)
		}
	}
}

// TestJournalReplay 不关闭直接重开（模拟异常退出），日志应被回放。
func TestJournalReplay(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b1 := []Entry{{Source: "persist me please", Target: "p1", CompletedAt: at(1)}}
	if err := s.WriteBatch(testPair, b1); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(at(1)); err != nil {
		t.Fatal(err)
	}
	// 不 Close，直接再开一个句柄读同一目录（日志已按批 flush）
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := s2.Search(testPair, "persist me please", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Target != "p1" {
		t.Fatalf("journal not replayed: %+v", hits)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	// s 不 Close（模拟进程退出）；其句柄随测试结束释放
	_ = s
}

func TestClosedStoreErrors(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != ErrClosed {
		t.Fatalf("second Close: got %v", err)
	}
	if err := s.WriteBatch(testPair, []Entry{{Source: "a", Target: "b", CompletedAt: at(1)}}); err != ErrClosed {
		t.Fatalf("WriteBatch after close: got %v", err)
	}
	if _, err := s.Search(testPair, "a", 0, 0); err != ErrClosed {
		t.Fatalf("Search after close: got %v", err)
	}
	if err := s.Cleanup(at(1)); err != ErrClosed {
		t.Fatalf("Cleanup after close: got %v", err)
	}
}

// TestSnapshotPlusJournal 快照之后的增量写入（未关闭）应在下次 Open 时
// 通过日志回放恢复，且与快照内容叠加。
func TestSnapshotPlusJournal(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b1 := []Entry{{Source: "in snapshot", Target: "s1", CompletedAt: at(1)}}
	if err := s.WriteBatch(testPair, b1); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// 重开后写入新条目但不关闭（模拟异常退出）
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b2 := []Entry{{Source: "only in journal", Target: "j1", CompletedAt: at(2)}}
	if err := s2.WriteBatch(testPair, b2); err != nil {
		t.Fatal(err)
	}
	// s2 不 Close，直接开第三个句柄
	s3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	for src, want := range map[string]string{"in snapshot": "s1", "only in journal": "j1"} {
		hits, err := s3.Search(testPair, src, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].Target != want {
			t.Fatalf("query %q: got %+v, want target %q", src, hits, want)
		}
	}
	_ = s2
}
