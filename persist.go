package tmem

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// 持久化布局（目录内）：
//   snapshot.dat —— Close 时写出的全量快照（存活条目 + trigram 索引）
//   journal.log  —— 运行期间的追加日志（写入/清理），Open 时回放
// Open 时若存在快照则先加载快照，再回放日志；Close 时写快照并删除日志。
// 快照先写临时文件再 rename，避免写一半留下损坏的快照。

const (
	snapshotFile = "snapshot.dat"
	snapshotTmp  = "snapshot.dat.tmp"
	journalFile  = "journal.log"

	snapshotMagic   = "TMEMSNAP"
	snapshotVersion = 1

	journalOpPut     = 1
	journalOpCleanup = 2
)

// ---------- 二进制读写辅助 ----------

type binWriter struct {
	w   *bufio.Writer
	buf [binary.MaxVarintLen64]byte
}

func (bw *binWriter) uvarint(v uint64) {
	n := binary.PutUvarint(bw.buf[:], v)
	bw.w.Write(bw.buf[:n])
}

func (bw *binWriter) uint64(v uint64) {
	binary.LittleEndian.PutUint64(bw.buf[:8], v)
	bw.w.Write(bw.buf[:8])
}

func (bw *binWriter) int64(v int64) { bw.uint64(uint64(v)) }

func (bw *binWriter) str(s string) {
	bw.uvarint(uint64(len(s)))
	bw.w.WriteString(s)
}

type binReader struct {
	r   *bufio.Reader
	buf [8]byte
}

func (br *binReader) uvarint() (uint64, error) {
	return binary.ReadUvarint(br.r)
}

func (br *binReader) uint64() (uint64, error) {
	if _, err := io.ReadFull(br.r, br.buf[:8]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(br.buf[:8]), nil
}

func (br *binReader) int64() (int64, error) {
	v, err := br.uint64()
	return int64(v), err
}

func (br *binReader) str() (string, error) {
	n, err := br.uvarint()
	if err != nil {
		return "", err
	}
	if n > 1<<22 { // 文本最长 1000 码点，留出余量；超限视为损坏
		return "", fmt.Errorf("tmem: corrupt file: string too long (%d)", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(br.r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

// ---------- 快照 ----------

func (s *Store) writeSnapshot(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	bw := &binWriter{w: w}
	w.WriteString(snapshotMagic)
	bw.uvarint(snapshotVersion)
	bw.uvarint(uint64(len(s.pairs)))
	for pair, ix := range s.pairs {
		// 先压缩：快照只含存活条目，id 连续，倒排表原样落盘。
		ix.compact()
		bw.str(pair.Source)
		bw.str(pair.Target)
		bw.uvarint(uint64(len(ix.live)))
		for id := range ix.entries {
			e := &ix.entries[id]
			bw.str(e.normSrc)
			bw.str(e.rawSrc)
			bw.str(e.target)
			bw.int64(e.completedAt)
		}
		// 索引：桶按长度、trigram 按哈希排序写出（确定性）。
		lens := make([]int, 0, len(ix.buckets))
		for l := range ix.buckets {
			lens = append(lens, l)
		}
		sort.Ints(lens)
		bw.uvarint(uint64(len(lens)))
		for _, l := range lens {
			b := ix.buckets[l]
			bw.uvarint(uint64(l))
			hashes := make([]uint64, 0, len(b.posts))
			for h := range b.posts {
				hashes = append(hashes, h)
			}
			sort.Slice(hashes, func(i, j int) bool { return hashes[i] < hashes[j] })
			bw.uvarint(uint64(len(hashes)))
			for _, h := range hashes {
				bw.uint64(h)
				p := b.posts[h]
				bw.uvarint(uint64(p.n))
				bw.uvarint(uint64(p.last))
				bw.uvarint(uint64(len(p.data)))
				bw.w.Write(p.data)
			}
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (s *Store) readSnapshot(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	br := &binReader{r: bufio.NewReaderSize(f, 1<<20)}
	magic := make([]byte, len(snapshotMagic))
	if _, err := io.ReadFull(br.r, magic); err != nil {
		return fmt.Errorf("tmem: corrupt snapshot: %w", err)
	}
	if string(magic) != snapshotMagic {
		return errors.New("tmem: corrupt snapshot: bad magic")
	}
	version, err := br.uvarint()
	if err != nil {
		return err
	}
	if version != snapshotVersion {
		return fmt.Errorf("tmem: unsupported snapshot version %d", version)
	}
	npairs, err := br.uvarint()
	if err != nil {
		return err
	}
	for p := uint64(0); p < npairs; p++ {
		var pair Pair
		if pair.Source, err = br.str(); err != nil {
			return err
		}
		if pair.Target, err = br.str(); err != nil {
			return err
		}
		ix := newPairIndex()
		s.pairs[pair] = ix
		nentries, err := br.uvarint()
		if err != nil {
			return err
		}
		for i := uint64(0); i < nentries; i++ {
			norm, err := br.str()
			if err != nil {
				return err
			}
			raw, err := br.str()
			if err != nil {
				return err
			}
			target, err := br.str()
			if err != nil {
				return err
			}
			at, err := br.int64()
			if err != nil {
				return err
			}
			id := int32(len(ix.entries))
			normLen := int32(len([]rune(norm)))
			ix.entries = append(ix.entries, memEntry{normSrc: norm, rawSrc: raw, target: target, completedAt: at, normLen: normLen})
			ix.live[norm] = id
			bk := bucketOf(int(normLen))
			b := ix.buckets[bk]
			if b == nil {
				b = &lengthBucket{posts: make(map[uint64]postingList)}
				ix.buckets[bk] = b
			}
			b.all = append(b.all, id)
		}
		nbuckets, err := br.uvarint()
		if err != nil {
			return err
		}
		for bi := uint64(0); bi < nbuckets; bi++ {
			l, err := br.uvarint()
			if err != nil {
				return err
			}
			b := ix.buckets[int(l)]
			if b == nil {
				b = &lengthBucket{posts: make(map[uint64]postingList)}
				ix.buckets[int(l)] = b
			}
			nkeys, err := br.uvarint()
			if err != nil {
				return err
			}
			for k := uint64(0); k < nkeys; k++ {
				h, err := br.uint64()
				if err != nil {
					return err
				}
				n, err := br.uvarint()
				if err != nil {
					return err
				}
				last, err := br.uvarint()
				if err != nil {
					return err
				}
				dlen, err := br.uvarint()
				if err != nil {
					return err
				}
				data := make([]byte, dlen)
				if _, err := io.ReadFull(br.r, data); err != nil {
					return err
				}
				b.posts[h] = postingList{data: data, last: int32(last), n: int32(n)}
			}
		}
	}
	return nil
}

// ---------- 日志 ----------

func (s *Store) appendJournalPut(pair Pair, rawSrc, target string, at int64) error {
	bw := &binWriter{w: s.journalW}
	bw.w.WriteByte(journalOpPut)
	bw.str(pair.Source)
	bw.str(pair.Target)
	bw.str(rawSrc)
	bw.str(target)
	bw.int64(at)
	return nil
}

func (s *Store) appendJournalCleanup(now int64) {
	bw := &binWriter{w: s.journalW}
	bw.w.WriteByte(journalOpCleanup)
	bw.int64(now)
}

func (s *Store) replayJournal(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	br := &binReader{r: bufio.NewReader(f)}
	for {
		op, err := br.r.ReadByte()
		if err != nil { // EOF 或读错误：结束回放
			return nil
		}
		switch op {
		case journalOpPut:
			var pair Pair
			var raw, target string
			var at int64
			if pair.Source, err = br.str(); err != nil {
				return nil // 尾部记录不完整（异常退出），忽略
			}
			if pair.Target, err = br.str(); err != nil {
				return nil
			}
			if raw, err = br.str(); err != nil {
				return nil
			}
			if target, err = br.str(); err != nil {
				return nil
			}
			if at, err = br.int64(); err != nil {
				return nil
			}
			norm, nerr := Normalize(raw)
			if nerr != nil {
				return nil // 日志内容损坏，停止回放
			}
			if raw == norm {
				s.applyPut(pair, norm, norm, target, at)
			} else {
				s.applyPut(pair, norm, raw, target, at)
			}
		case journalOpCleanup:
			now, err := br.int64()
			if err != nil {
				return nil
			}
			s.applyCleanup(now)
		default:
			return nil // 无法识别的记录：停止回放
		}
	}
}

// ---------- 顶层 load/save ----------

func (s *Store) load() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	// 清理可能残留的临时快照
	os.Remove(filepath.Join(s.dir, snapshotTmp))
	snap := filepath.Join(s.dir, snapshotFile)
	if _, err := os.Stat(snap); err == nil {
		if err := s.readSnapshot(snap); err != nil {
			return err
		}
	}
	if err := s.replayJournal(filepath.Join(s.dir, journalFile)); err != nil {
		return err
	}
	// 以追加模式打开日志，供本次会话写入
	jf, err := os.OpenFile(filepath.Join(s.dir, journalFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	s.journal = jf
	s.journalW = bufio.NewWriterSize(jf, 1<<16)
	return nil
}

func (s *Store) save() error {
	tmp := filepath.Join(s.dir, snapshotTmp)
	if err := s.writeSnapshot(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, snapshotFile)); err != nil {
		return err
	}
	// 快照落盘后日志即可丢弃
	if s.journal != nil {
		s.journalW.Flush()
		s.journal.Close()
		s.journal = nil
		s.journalW = nil
	}
	os.Remove(filepath.Join(s.dir, journalFile))
	return nil
}
