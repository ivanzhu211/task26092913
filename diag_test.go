package tmem

import (
	"math/rand"
	"os"
	"testing"
	"time"
)

// 诊断：10 万条规模下统计候选数量与分阶段耗时。TMEM_PERF=1 时运行。
func TestDiagCandidates(t *testing.T) {
	if os.Getenv("TMEM_PERF") == "" {
		t.Skip("set TMEM_PERF=1")
	}
	rng := rand.New(rand.NewSource(99))
	vocab := perfVocab(rng)
	ix := newPairIndex()
	var bases []string
	const n = 100000
	for i := 0; i < n; i++ {
		var src string
		if len(bases) > 100 && rng.Intn(2) == 0 {
			src = perfMutate(rng, vocab, bases[rng.Intn(len(bases))])
		} else {
			src = perfSentence(rng, vocab)
			if len(bases) < 20000 {
				bases = append(bases, src)
			}
		}
		norm, _ := Normalize(src)
		ix.add(norm, norm, "T:"+norm, int64(i))
	}
	t.Logf("entries=%d buckets=%d", len(ix.entries), len(ix.buckets))
	// 倒排表规模分布
	total, max, keys := 0, 0, 0
	for _, b := range ix.buckets {
		for _, p := range b.posts {
			total += int(p.n)
			keys++
			if int(p.n) > max {
				max = int(p.n)
			}
		}
	}
	t.Logf("postings: keys=%d total=%d avg=%.1f max=%d", keys, total, float64(total)/float64(keys), max)

	var sc searchScratch
	for _, q := range []string{bases[0], bases[100], perfMutate(rng, vocab, bases[50])} {
		qr := []rune(q)
		// 统计每个桶的计数分布
		L := len(qr)
		lo, hi := lengthRange(L, 75)
		for l := lo; l <= hi; l++ {
			b := ix.buckets[l]
			if b == nil {
				continue
			}
			m := L
			if l > m {
				m = l
			}
			dmax := m * 25 / 100
			need := m - 2 - 3*dmax
			if need <= 0 {
				continue
			}
			counts := map[int32]int{}
			for i := 0; i+3 <= L; i++ {
				h := trigramHash(qr[i], qr[i+1], qr[i+2])
				p, ok := b.posts[h]
				if !ok {
					continue
				}
				data := p.data
				id := int32(0)
				for len(data) > 0 {
					v, k := 0, 0
					// 内联 uvarint 解码
					for {
						c := data[k]
						k++
						v |= int(c&0x7f) << (7 * uint(k-1))
						if c < 0x80 {
							break
						}
					}
					data = data[k:]
					id += int32(v >> posBits)
					pos := v & (1<<posBits - 1)
					if pos > i+dmax || pos < i-dmax {
						continue
					}
					counts[id]++
				}
			}
			pass, maxC, sum := 0, 0, 0
			for _, c := range counts {
				sum += c
				if c > maxC {
					maxC = c
				}
				if c >= need {
					pass++
				}
			}
			if l == L || l == lo || pass > 0 {
				t.Logf("  bucket l=%d need=%d touched=%d avgCnt=%.1f maxCnt=%d pass=%d",
					l, need, len(counts), float64(sum)/float64(len(counts)+1), maxC, pass)
			}
		}
		start := time.Now()
		cands := ix.candidates(qr, 75, &sc)
		d1 := time.Since(start)
		start = time.Now()
		ver := 0
		for _, id := range cands {
			er := []rune(ix.entries[id].normSrc)
			m := len(qr)
			if len(er) > m {
				m = len(er)
			}
			d := editDistanceBounded(qr, er, m*(100-75)/100)
			if d <= m*(100-75)/100 {
				ver++
			}
		}
		t.Logf("q(len=%d): candidates=%d candTime=%v verify=%d verifyTime=%v",
			len(qr), len(cands), d1, ver, time.Since(start))
	}
}
