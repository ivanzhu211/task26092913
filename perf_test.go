package tmem

import (
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 百万级性能验证：默认跳过，用 TMEM_PERF=1 go test -run TestPerfMillion -v 执行。
// 场景：单语言对 100 万条，原文 20~300 码点，用词集中，大量近重复句。

const (
	perfEntries   = 1_000_000
	perfBatch     = 1000
	perfQueries   = 3000
	perfDailyW    = 5000 // 一年模拟中每天写入量
	perfVocabSize = 3000
)

// perfVocab 生成用词集中的语料：3000 个词形、按英文式字母频率随机拼成，
// 词长 2~12。字母分布真实，保证字符 trigram 多样性接近真实文本。
func perfVocab(rng *rand.Rand) []string {
	const letters = "eeeeeeeeeeeeaaaaaaaaaiiiiiiiioooooooonnnnnnrrrrrrttttttllllssssuuuuddddgggbbccmmppffhhvvwwyykjxqz"
	vocab := make([]string, perfVocabSize)
	for i := range vocab {
		n := 2 + rng.Intn(11)
		var b strings.Builder
		for j := 0; j < n; j++ {
			b.WriteByte(letters[rng.Intn(len(letters))])
		}
		vocab[i] = b.String()
	}
	return vocab
}

func perfSentence(rng *rand.Rand, vocab []string) string {
	target := 20 + rng.Intn(281) // 20~300 码点
	var b strings.Builder
	for b.Len() < target {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(vocab[rng.Intn(len(vocab))])
	}
	return b.String()
}

// perfMutate 改 1~3 个词，模拟“只差几个词”的句子。
func perfMutate(rng *rand.Rand, vocab []string, s string) string {
	words := strings.Split(s, " ")
	n := 1 + rng.Intn(3)
	for k := 0; k < n && len(words) > 0; k++ {
		switch rng.Intn(3) {
		case 0:
			words[rng.Intn(len(words))] = vocab[rng.Intn(len(vocab))]
		case 1:
			i := rng.Intn(len(words))
			words = append(words[:i], words[i+1:]...)
		case 2:
			i := rng.Intn(len(words) + 1)
			words = append(words[:i], append([]string{vocab[rng.Intn(len(vocab))]}, words[i:]...)...)
		}
	}
	out := strings.Join(words, " ")
	if strings.TrimSpace(out) == "" {
		return s
	}
	return out
}

func peakRSSMB() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return -1
	}
	// darwin 上 Maxrss 单位是字节，Linux 上是 KB
	if runtime.GOOS == "darwin" {
		return ru.Maxrss >> 20
	}
	return ru.Maxrss >> 10
}

func memReport(t *testing.T, label string) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t.Logf("[%s] HeapAlloc=%dMB HeapSys=%dMB PeakRSS=%dMB", label,
		ms.HeapAlloc>>20, ms.HeapSys>>20, peakRSSMB())
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	i := int(p*float64(len(sorted))+0.5) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func runSearches(t *testing.T, s *Store, queries []string, label string) []time.Duration {
	lat := make([]time.Duration, 0, len(queries))
	totalHits := 0
	for _, q := range queries {
		start := time.Now()
		hits, err := s.Search(testPair, q, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		lat = append(lat, time.Since(start))
		totalHits += len(hits)
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	t.Logf("[%s] n=%d p50=%v p95=%v p99=%v max=%v avgHits=%.1f",
		label, len(lat), percentile(lat, 0.50), percentile(lat, 0.95),
		percentile(lat, 0.99), lat[len(lat)-1], float64(totalHits)/float64(len(lat)))
	return lat
}

func TestPerfMillion(t *testing.T) {
	if os.Getenv("TMEM_PERF") == "" {
		t.Skip("set TMEM_PERF=1 to run the million-entry perf test")
	}
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(99))
	vocab := perfVocab(rng)
	pair := testPair

	// ---- 写入 100 万条 ----
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var bases []string // 留样的句子，用于生成查询与近重复
	writeStart := time.Now()
	maxBatch := time.Duration(0)
	written := 0
	day := 0
	for written < perfEntries {
		batch := make([]Entry, perfBatch)
		for i := range batch {
			var src string
			if len(bases) > 100 && rng.Intn(2) == 0 {
				src = perfMutate(rng, vocab, bases[rng.Intn(len(bases))])
			} else {
				src = perfSentence(rng, vocab)
				if len(bases) < 60000 {
					bases = append(bases, src)
				}
			}
			// 初始语料的完成时间均匀分布在过去 180 天（模拟历史积累），
			// 避免全部堆在 day 0 导致第 180 天出现双倍存活峰值。
			batch[i] = Entry{Source: src, Target: "T:" + src[:len(src)/2], CompletedAt: at(day - rng.Intn(180))}
		}
		bs := time.Now()
		if err := s.WriteBatch(pair, batch); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(bs); d > maxBatch {
			maxBatch = d
		}
		written += len(batch)
	}
	t.Logf("write 1M: total=%v maxBatch(1000)=%v", time.Since(writeStart), maxBatch)
	memReport(t, "after 1M writes")

	// ---- 检索延迟 ----
	queries := make([]string, perfQueries)
	for i := range queries {
		r := rng.Intn(10)
		switch {
		case r < 6: // 近重复
			queries[i] = perfMutate(rng, vocab, bases[rng.Intn(len(bases))])
		case r < 9: // 原句
			queries[i] = bases[rng.Intn(len(bases))]
		default: // 随机新句
			queries[i] = perfSentence(rng, vocab)
		}
	}
	// 预热
	runSearches(t, s, queries[:100], "warmup")
	runSearches(t, s, queries, "search-1M")

	// ---- 关闭重开：30 秒内可检索且结果一致 ----
	checkQ := queries[:200]
	before := make([][]Hit, len(checkQ))
	for i, q := range checkQ {
		h, err := s.Search(pair, q, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		before[i] = h
	}
	closeStart := time.Now()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("close: %v", time.Since(closeStart))

	openStart := time.Now()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reopen: %v", time.Since(openStart))
	for i, q := range checkQ {
		got, err := s.Search(pair, q, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !equalHits(got, before[i]) {
			t.Fatalf("reopen mismatch for query %q:\n got %+v\nwant %+v", q, got, before[i])
		}
	}
	t.Log("reopen: 200 queries identical to pre-close results")

	// ---- 模拟一年：每天写入 + 每晚清理 ----
	simStart := time.Now()
	for d := 1; d <= 365; d++ {
		day++
		for w := 0; w < perfDailyW; w += perfBatch {
			batch := make([]Entry, perfBatch)
			for i := range batch {
				src := perfMutate(rng, vocab, bases[rng.Intn(len(bases))])
				batch[i] = Entry{Source: src, Target: "T:" + src[:len(src)/2], CompletedAt: at(day)}
			}
			if err := s.WriteBatch(pair, batch); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Cleanup(at(day)); err != nil {
			t.Fatal(err)
		}
		if d%90 == 0 {
			t.Logf("sim day %d done (%v)", d, time.Since(simStart))
			memReport(t, fmt.Sprintf("sim day %d", d))
		}
	}
	t.Logf("1-year simulation: %v", time.Since(simStart))
	memReport(t, "after 1-year sim")
	runSearches(t, s, queries, "search-after-1y")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
