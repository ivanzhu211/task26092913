package tmem

// editDistanceBounded 计算两段码点序列的 Levenshtein 编辑距离
// （插入、删除、替换一个码点各算 1）。若真实距离大于 maxDist，
// 返回值大于 maxDist（可能提前退出，不保证等于真实距离）。
// maxDist < 0 表示不设上界。
func editDistanceBounded(a, b []rune, maxDist int) int {
	if len(a) > len(b) {
		a, b = b, a
	}
	// 现在 len(a) <= len(b)
	la, lb := len(a), len(b)
	if lb-la > maxDist && maxDist >= 0 {
		return maxDist + 1
	}
	if la == 0 {
		return lb
	}
	prev := make([]int, la+1)
	cur := make([]int, la+1)
	for i := 0; i <= la; i++ {
		prev[i] = i
	}
	for j := 1; j <= lb; j++ {
		cur[0] = j
		rowMin := cur[0]
		for i := 1; i <= la; i++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			v := prev[i] + 1 // 删除 b[j-1]
			if x := cur[i-1] + 1; x < v {
				v = x // 插入
			}
			if x := prev[i-1] + cost; x < v {
				v = x // 替换或匹配
			}
			cur[i] = v
			if v < rowMin {
				rowMin = v
			}
		}
		if maxDist >= 0 && rowMin > maxDist {
			return maxDist + 1
		}
		prev, cur = cur, prev
	}
	return prev[la]
}

// similarity 由编辑距离 d 和较长段长度 m 计算相似度：floor(100*(m-d)/m)。
// 调用方保证 0 <= d <= m 且 m > 0。
func similarity(d, m int) int {
	return 100 * (m - d) / m
}
