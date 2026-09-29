package tmem

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

var testPair = Pair{Source: "en", Target: "es"}

func mustWrite(t *testing.T, s *Store, pair Pair, entries ...Entry) {
	t.Helper()
	if err := s.WriteBatch(pair, entries); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}
}

func at(day int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(day) * 24 * time.Hour)
}

func TestTinyAndExactThreshold(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := newBruteModel()

	// 极短原文 + 相似度正好落在门槛 75 上（m=4, d=1 -> 75）
	batch := []Entry{
		{Source: "a", Target: "uno", CompletedAt: at(1)},
		{Source: "ab", Target: "dos", CompletedAt: at(1)},
		{Source: "abcd", Target: "cuatro", CompletedAt: at(2)},
		{Source: "abce", Target: "cuatro-bis", CompletedAt: at(3)},
		{Source: "wxyz", Target: "otro", CompletedAt: at(4)},
	}
	mustWrite(t, s, testPair, batch...)
	m.write(testPair, batch)

	for _, th := range []int{0, 75, 76, 99, 100} {
		for _, q := range []string{"a", "ab", "abc", "abcd", "abce", "abcde", ""} {
			if q == "" {
				continue // 空查询单独测
			}
			checkSearch(t, s, m, testPair, q, th, 0)
			checkSearch(t, s, m, testPair, q, th, 1)
			checkSearch(t, s, m, testPair, q, th, 50)
		}
	}
}

func TestLengthGapAndMultilingual(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := newBruteModel()

	long := "the quick brown fox jumps over the lazy dog near the river bank every single morning"
	batch := []Entry{
		{Source: long, Target: "largo", CompletedAt: at(1)},
		// 长度差很大但相似度 >= 75（删掉 25% 的词）
		{Source: "the quick brown fox jumps over the lazy dog near the river bank every", Target: "corto", CompletedAt: at(2)},
		{Source: "条约第一条　缔约双方　应当遵守", Target: "条约一", CompletedAt: at(3)},
		{Source: "条约第一条 缔约双方 应当遵守", Target: "条约一-重复", CompletedAt: at(4)}, // 规范化后相同，覆盖
		{Source: "مرحبا بالعالم هذه تجربة", Target: "ar1", CompletedAt: at(5)},
		{Source: "مرحبا بالعالم هذه تجربةٌ", Target: "ar2", CompletedAt: at(6)},
		{Source: "mixed\u00a0\u00a0whitespace　 here\tand\nthere", Target: "ws", CompletedAt: at(7)},
	}
	mustWrite(t, s, testPair, batch...)
	m.write(testPair, batch)

	queries := []string{
		long,
		"the quick brown fox jumps over the lazy dog near the river bank every single morning!",
		"条约第一条 缔约双方 应当遵守",
		"条约第一条　　缔约双方　　应当遵守 ", // 混合空白，规范化后相同
		"مرحبا بالعالم هذه تجربة",
		"mixed whitespace here and there",
		"mixed\u00a0whitespace　here  and\tthere",
	}
	for _, q := range queries {
		for _, th := range []int{75, 90, 100} {
			checkSearch(t, s, m, testPair, q, th, 0)
			checkSearch(t, s, m, testPair, q, th, 3)
		}
	}
}

func TestOverwriteAndCleanup(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := newBruteModel()

	b1 := []Entry{{Source: "hello world", Target: "hola", CompletedAt: at(0)}}
	mustWrite(t, s, testPair, b1...)
	m.write(testPair, b1)
	// 覆盖：译文与时间更新
	b2 := []Entry{{Source: "hello world", Target: "hola2", CompletedAt: at(10)}}
	mustWrite(t, s, testPair, b2...)
	m.write(testPair, b2)
	// 批内重复：以靠后的为准
	b3 := []Entry{
		{Source: "foo bar", Target: "t1", CompletedAt: at(20)},
		{Source: "foo  bar", Target: "t2", CompletedAt: at(21)}, // 规范化后相同
	}
	mustWrite(t, s, testPair, b3...)
	m.write(testPair, b3)

	checkSearch(t, s, m, testPair, "hello world", 75, 0)
	checkSearch(t, s, m, testPair, "foo bar", 75, 0)

	// 清理：now=at(200)，cutoff=at(20)；at(0)/at(10) 删除，at(20) 正好 180 天保留，at(21) 保留
	now := at(200)
	if err := s.Cleanup(now); err != nil {
		t.Fatal(err)
	}
	m.cleanup(now)
	for _, q := range []string{"hello world", "foo bar", "hello world!", "foo bars"} {
		checkSearch(t, s, m, testPair, q, 75, 0)
		checkSearch(t, s, m, testPair, q, 75, 50)
	}
}

func TestWriteBatchAtomicity(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	good := Entry{Source: "ok", Target: "bien", CompletedAt: at(1)}
	bad := []Entry{
		good,
		{Source: "bad\xffutf8", Target: "x", CompletedAt: at(1)},
	}
	if err := s.WriteBatch(testPair, bad); err == nil {
		t.Fatal("expected error for invalid batch")
	}
	// 整批未写入
	hits, err := s.Search(testPair, "ok", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("invalid batch partially written: %+v", hits)
	}
	// 空 target 也不合法
	if err := s.WriteBatch(testPair, []Entry{{Source: "src", Target: "  ", CompletedAt: at(1)}}); err == nil {
		t.Fatal("expected error for empty target")
	}
}

func TestInvalidSearchParams(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Search(testPair, "q", 74, 0); err != ErrInvalidThreshold {
		t.Fatalf("th=74: got %v", err)
	}
	if _, err := s.Search(testPair, "q", 101, 0); err != ErrInvalidThreshold {
		t.Fatalf("th=101: got %v", err)
	}
	if _, err := s.Search(testPair, "q", 0, 51); err != ErrInvalidLimit {
		t.Fatalf("lim=51: got %v", err)
	}
	if _, err := s.Search(testPair, "   ", 0, 0); err != ErrInvalidText {
		t.Fatalf("blank query: got %v", err)
	}
	if _, err := s.Search(testPair, "bad\xff", 0, 0); err != ErrInvalidText {
		t.Fatalf("invalid utf8 query: got %v", err)
	}
}

// TestRandomizedDifferential 随机增删查，与暴力参考实现逐次对照。
func TestRandomizedDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := newBruteModel()

	vocab := []string{"the", "treaty", "of", "peace", "shall", "be", "signed", "by", "both", "parties",
		"article", "one", "two", "state", "rights", "asylum", "seeker", "policy", "政府", "条约", "缔约", "مرحبا", "بالعالم"}
	pairs := []Pair{testPair, {Source: "zh", Target: "en"}, {Source: "ar", Target: "ar"}}

	randSentence := func() string {
		n := 1 + rng.Intn(12)
		words := make([]string, n)
		for i := range words {
			words[i] = vocab[rng.Intn(len(vocab))]
		}
		out := words[0]
		for _, w := range words[1:] {
			sep := " "
			if rng.Intn(20) == 0 {
				sep = "  \u00a0" // 偶尔混入特殊空白
			}
			out += sep + w
		}
		return out
	}
	mutate := func(src string) string {
		r := []rune(src)
		if len(r) == 0 {
			return src
		}
		switch rng.Intn(3) {
		case 0: // 替换一个码点
			r[rng.Intn(len(r))] = []rune("xy条")[rng.Intn(3)]
		case 1: // 删除一个码点
			i := rng.Intn(len(r))
			r = append(r[:i], r[i+1:]...)
		case 2: // 插入一个码点
			i := rng.Intn(len(r) + 1)
			r = append(r[:i], append([]rune{'z'}, r[i:]...)...)
		}
		return string(r)
	}

	day := 0
	for round := 0; round < 60; round++ {
		day += rng.Intn(10)
		pair := pairs[rng.Intn(len(pairs))]
		switch rng.Intn(10) {
		case 0, 1, 2, 3, 4, 5: // 写入一批
			n := 1 + rng.Intn(8)
			batch := make([]Entry, n)
			for i := range batch {
				batch[i] = Entry{
					Source:      randSentence(),
					Target:      fmt.Sprintf("T%d-%d", round, i),
					CompletedAt: at(day),
				}
			}
			if err := s.WriteBatch(pair, batch); err != nil {
				t.Fatalf("round %d write: %v", round, err)
			}
			m.write(pair, batch)
		case 6: // 清理
			now := at(day)
			if err := s.Cleanup(now); err != nil {
				t.Fatal(err)
			}
			m.cleanup(now)
		default: // 检索并对照
			q := randSentence()
			if rng.Intn(2) == 0 {
				q = mutate(q)
			}
			th := []int{0, 75, 80, 90, 95, 100}[rng.Intn(6)]
			lim := []int{0, 1, 3, 5, 50}[rng.Intn(5)]
			checkSearch(t, s, m, pair, q, th, lim)
		}
	}
}
