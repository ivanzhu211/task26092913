package tmem

import "encoding/binary"

// 本文件实现单个语言对的内存索引。
//
// 设计：条目按规范化原文的码点长度分桶（16 个长度一桶）；桶内对三元组
// （trigram，按码点）建位置感知的倒排索引（trigram 哈希 -> 差分 varint 编码
// 的升序 (条目id, 位置) 序列）。检索时：
//  1. 长度过滤：|L-l| <= floor(max(L,l)*(100-th)/100) 之外的条目直接排除；
//  2. 位置感知的 trigram 计数过滤：由位置 q-gram 引理，编辑距离 <= dmax 时，
//     位置差不超过 dmax 的公共 trigram 数 >= max(L,l)-2-3*dmax（必要条件）。
//     计数按桶扫描、按条目实际长度分别套用下界；超长倒排表跳过不计，同时
//     等量减少下界（每个被跳过的查询 trigram 至多对应 1 个幸存匹配对），
//     保证只误留、不误删；
//  3. 对候选做精确（有界）编辑距离验证。
// 哈希冲突与重复计数都只增加候选，最终结果与逐条暴力比对完全一致。
//
// 覆盖写入与清理采用墓碑标记，不物理删除倒排表项；墓碑比例过高时原地压缩，
// Close 写快照时也会压缩。

const trigramQ = 3

// bucketShift 决定长度桶宽度（2^bucketShift 个码点长度一桶）。
const bucketShift = 4

// maxScanList 是计数时单个倒排表允许扫描的最大长度，超出的表跳过并下调下界。
// 取值让真实语料中常见的空格 trigram（数万条）都能扫描，只防病态数据。
const maxScanList = 65536

// posBits 是倒排表项中位置占用的位数；位置最大 997（1000 码点 - 3），10 位足够。
const posBits = 10

// memEntry 是内存中的条目。被覆盖或被清理的条目保留为墓碑（deleted=true）。
type memEntry struct {
	normSrc     string // 规范化原文（去重键、排序键、索引内容）
	rawSrc      string // 最后一次写入时的原文原样（与 normSrc 相等时共享底层字节）
	target      string // 最后一次写入时的译文原样
	completedAt int64  // 最后写入时间（Unix 纳秒）
	normLen     int32  // normSrc 的码点数
	deleted     bool
}

// postingList 是升序条目 id 的差分 uvarint 编码序列，每项同时携带 trigram
// 在条目中的起始位置（内存约为 []int32 的 1/3）。
type postingList struct {
	data []byte
	last int32
	n    int32
}

func (p *postingList) add(id int32, pos int) {
	p.data = binary.AppendUvarint(p.data, uint64(id-p.last)<<posBits|uint64(pos))
	p.last = id
	p.n++
}

// lengthBucket 是一个长度区间内条目的 trigram 倒排表。
type lengthBucket struct {
	all   []int32                // 桶内全部条目 id（含无 trigram 的短条目）
	posts map[uint64]postingList // trigram 哈希 -> 升序 (条目id, 位置)
}

type pairIndex struct {
	entries []memEntry            // id -> 条目（含墓碑）
	live    map[string]int32      // 规范化原文 -> 存活条目 id
	buckets map[int]*lengthBucket // 长度区间 -> 桶
}

func newPairIndex() *pairIndex {
	return &pairIndex{
		live:    make(map[string]int32),
		buckets: make(map[int]*lengthBucket),
	}
}

func bucketOf(runelength int) int { return runelength >> bucketShift }

// trigramHash 计算三个码点的哈希。确定性即可，冲突只增加候选。
func trigramHash(a, b, c rune) uint64 {
	h := uint64(uint32(a)) // 码点 <= 0x10FFFF，uint32 安全
	h = h*0x100000001b3 ^ uint64(uint32(b))
	h = h*0x100000001b3 ^ uint64(uint32(c))
	return h
}

// add 把条目加入索引，返回新条目 id。调用方保证 normSrc 当前无存活条目。
func (ix *pairIndex) add(normSrc, rawSrc, target string, completedAt int64) int32 {
	runes := []rune(normSrc)
	id := int32(len(ix.entries))
	ix.entries = append(ix.entries, memEntry{
		normSrc:     normSrc,
		rawSrc:      rawSrc,
		target:      target,
		completedAt: completedAt,
		normLen:     int32(len(runes)),
	})
	ix.live[normSrc] = id
	l := len(runes)
	bk := bucketOf(l)
	b := ix.buckets[bk]
	if b == nil {
		b = &lengthBucket{posts: make(map[uint64]postingList)}
		ix.buckets[bk] = b
	}
	b.all = append(b.all, id)
	for i := 0; i+trigramQ <= l; i++ {
		h := trigramHash(runes[i], runes[i+1], runes[i+2])
		p := b.posts[h]
		p.add(id, i)
		b.posts[h] = p
	}
	return id
}

// kill 把条目标记为墓碑（覆盖写入或清理时调用）。
func (ix *pairIndex) kill(id int32) {
	e := &ix.entries[id]
	if e.deleted {
		return
	}
	e.deleted = true
	delete(ix.live, e.normSrc)
	// 释放文本引用，墓碑只保留到下次压缩
	e.normSrc = ""
	e.rawSrc = ""
	e.target = ""
}

// shouldCompact 判断墓碑是否多到值得原地压缩。
func (ix *pairIndex) shouldCompact() bool {
	dead := len(ix.entries) - len(ix.live)
	return dead > 50000 && dead > len(ix.live)/4
}

// compact 原地清除墓碑：重排条目 id、过滤并重编码倒排表。
// 倒排表在同一切片内原地重写（写游标不超过读游标），避免压缩期间内存翻倍；
// 容量明显过剩的切片重新分配为刚好大小，避免长期持有冗余内存。
// 重映射是单调的，倒排表保持升序。
func (ix *pairIndex) compact() {
	remap := make([]int32, len(ix.entries))
	newEntries := make([]memEntry, 0, len(ix.live))
	for id := range ix.entries {
		if ix.entries[id].deleted {
			remap[id] = -1
		} else {
			remap[id] = int32(len(newEntries))
			newEntries = append(newEntries, ix.entries[id])
		}
	}
	ix.entries = newEntries
	if cap(ix.entries) > 2*len(ix.entries)+1024 {
		exact := make([]memEntry, len(ix.entries))
		copy(exact, ix.entries)
		ix.entries = exact
	}
	for norm, id := range ix.live {
		ix.live[norm] = remap[id]
	}
	var buf [binary.MaxVarintLen64]byte
	for bk, b := range ix.buckets {
		all := b.all[:0]
		for _, id := range b.all {
			if remap[id] >= 0 {
				all = append(all, remap[id])
			}
		}
		b.all = all
		if cap(b.all) > 2*len(b.all)+64 {
			exact := make([]int32, len(b.all))
			copy(exact, b.all)
			b.all = exact
		}
		for h, p := range b.posts {
			// 解码 -> 过滤 -> 重编码，原地写回 p.data
			out := p.data[:0]
			last := int32(0)
			n := int32(0)
			data := p.data
			cur := int32(0)
			for len(data) > 0 {
				v, k := binary.Uvarint(data)
				if k <= 0 {
					break // 不应发生；防御性退出
				}
				data = data[k:]
				cur += int32(v >> posBits)
				if remap[cur] >= 0 {
					nid := remap[cur]
					m := binary.PutUvarint(buf[:], uint64(nid-last)<<posBits|(v&(1<<posBits-1)))
					out = append(out, buf[:m]...)
					last = nid
					n++
				}
			}
			if n == 0 {
				delete(b.posts, h)
			} else {
				if cap(out) > 2*len(out)+64 {
					exact := make([]byte, len(out))
					copy(exact, out)
					out = exact
				}
				p.data = out
				p.last = last
				p.n = n
				b.posts[h] = p
			}
		}
		if len(b.all) == 0 {
			delete(ix.buckets, bk)
		}
	}
}

// searchScratch 是检索用的可复用计数缓冲：gc 高 32 位是扫描代次、低 32 位是
// 当前代 trigram 命中数，一次内存访问完成代次校验与计数。
type searchScratch struct {
	gc      []uint64
	gen     uint64
	touched []int32 // 当前代被碰过的条目 id
}

func (sc *searchScratch) reset(n int) {
	if cap(sc.gc) < n {
		sc.gc = make([]uint64, n)
	} else {
		sc.gc = sc.gc[:n]
	}
	sc.touched = sc.touched[:0]
}

// lengthRange 计算门槛 th 下，与长度 L 可能满足相似度的候选长度区间 [lo, hi]。
// 必要条件：|L-l| <= floor(max(L,l)*(100-th)/100)。
func lengthRange(L, th int) (lo, hi int) {
	dmaxAtL := L * (100 - th) / 100
	lo = L - dmaxAtL
	if lo < 1 {
		lo = 1
	}
	hi = L
	for l := L + 1; ; l++ {
		if l-L > l*(100-th)/100 {
			break
		}
		hi = l
	}
	return lo, hi
}

// candidates 返回通过长度与 trigram 计数过滤的存活条目 id（未做精确验证）。
func (ix *pairIndex) candidates(q []rune, th int, sc *searchScratch) []int32 {
	L := len(q)
	sc.reset(len(ix.entries))
	lo, hi := lengthRange(L, th)
	var out []int32
	for bk := bucketOf(lo); bk <= bucketOf(hi); bk++ {
		b := ix.buckets[bk]
		if b == nil {
			continue
		}
		// 位置窗口用本桶可能出现的最大 dmax（只放宽、不漏匹配）。
		lmax := (bk+1)<<bucketShift - 1
		if lmax > hi {
			lmax = hi
		}
		m := L
		if lmax > m {
			m = lmax
		}
		dmaxWin := m * (100 - th) / 100
		sc.gen++
		sc.touched = sc.touched[:0]
		gen := sc.gen << 32
		if L >= trigramQ {
			skipped := 0
			for i := 0; i+trigramQ <= L; i++ {
				h := trigramHash(q[i], q[i+1], q[i+2])
				p, ok := b.posts[h]
				if !ok {
					continue
				}
				if p.n > maxScanList {
					skipped++ // 该查询 trigram 不扫描，下界相应减一
					continue
				}
				data := p.data
				id := int32(0)
				plo := i - dmaxWin
				phi := i + dmaxWin
				for len(data) > 0 {
					// 内联 uvarint 解码（多数 1~3 字节）
					c := data[0]
					v := uint64(c & 0x7f)
					k := 1
					for c >= 0x80 {
						c = data[k]
						v |= uint64(c&0x7f) << (7 * uint(k))
						k++
					}
					data = data[k:]
					id += int32(v >> posBits)
					pos := int(v & (1<<posBits - 1))
					if pos < plo || pos > phi {
						continue // 位置差超过窗口的匹配不计入
					}
					g := sc.gc[id]
					if g>>32 != sc.gen {
						sc.gc[id] = gen | 1
						sc.touched = append(sc.touched, id)
					} else {
						sc.gc[id] = g + 1
					}
				}
			}
			// 按条目实际长度分别套用下界
			for _, id := range sc.touched {
				e := &ix.entries[id]
				if e.deleted {
					continue
				}
				l := int(e.normLen)
				if l < lo || l > hi {
					continue
				}
				mm := L
				if l > mm {
					mm = l
				}
				need := mm - (trigramQ - 1) - trigramQ*(mm*(100-th)/100) - skipped
				if need <= 0 || int(uint32(sc.gc[id])) >= need {
					out = append(out, id)
				}
			}
		} else {
			// 查询没有 trigram：长度区间内存活条目都是候选
			for _, id := range b.all {
				e := &ix.entries[id]
				if e.deleted {
					continue
				}
				if l := int(e.normLen); l >= lo && l <= hi {
					out = append(out, id)
				}
			}
		}
	}
	return out
}
