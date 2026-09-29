package tmem

import (
	"errors"
	"time"
)

// Pair 表示一个语言对，语言代码按原样区分（不做大小写或规范化处理）。
type Pair struct {
	Source string
	Target string
}

// Entry 是写入用的条目：一对原文和译文，以及调用方给出的完成时间。
type Entry struct {
	Source      string    // 原文（原样保存，检索时按规范化后的形式匹配）
	Target      string    // 译文（原样保存）
	CompletedAt time.Time // 完成时间，作为该条目的最后写入时间
}

// Hit 是一条检索结果。
type Hit struct {
	Source     string // 最后一次写入时的原文原样
	Target     string // 最后一次写入时的译文
	Similarity int    // 与检索原文的相似度（0-100 的整数）
}

const (
	// MaxTextRunes 是规范化后文本允许的最大码点数。
	MaxTextRunes = 1000

	DefaultThreshold = 75 // 默认相似度门槛
	MinThreshold     = 75
	MaxThreshold     = 100

	DefaultLimit = 5 // 默认返回条数上限
	MinLimit     = 1
	MaxLimit     = 50
)

var (
	// ErrInvalidText 表示文本不合法：非合法 UTF-8、规范化后为空或超过 MaxTextRunes 个码点。
	ErrInvalidText = errors.New("tmem: invalid text")
	// ErrInvalidThreshold 表示门槛不在 [MinThreshold, MaxThreshold] 内。
	ErrInvalidThreshold = errors.New("tmem: invalid threshold")
	// ErrInvalidLimit 表示条数上限不在 [MinLimit, MaxLimit] 内。
	ErrInvalidLimit = errors.New("tmem: invalid limit")
	// ErrClosed 表示在已关闭的 Store 上调用。
	ErrClosed = errors.New("tmem: store is closed")
)
