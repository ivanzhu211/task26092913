package tmem

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Normalize 对文本做规范化：连续的 Unicode 空白字符（含不换行空格、
// 全角空格等）合并为一个半角空格，并去掉首尾空白。不做大小写等其他变换。
// 若输入不是合法 UTF-8、规范化后为空或超过 MaxTextRunes 个码点，
// 返回 ErrInvalidText。
func Normalize(text string) (string, error) {
	if !utf8.ValidString(text) {
		return "", ErrInvalidText
	}
	var b strings.Builder
	b.Grow(len(text))
	inSpace := false // 已遇到空白、尚未写入非空白字符
	wrote := false   // 已写入非空白字符
	runes := 0
	for _, r := range text {
		if unicode.IsSpace(r) {
			inSpace = true
			continue
		}
		if inSpace && wrote {
			b.WriteByte(' ')
		}
		inSpace = false
		wrote = true
		b.WriteRune(r)
		runes++
		if runes > MaxTextRunes {
			return "", ErrInvalidText
		}
	}
	if runes == 0 {
		return "", ErrInvalidText
	}
	return b.String(), nil
}
