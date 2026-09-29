package tmem

import (
	"math/rand"
	"testing"
)

// 参考实现：无界、朴素 DP。
func levRef(a, b []rune) int {
	la, lb := len(a), len(b)
	dp := make([][]int, la+1)
	for i := range dp {
		dp[i] = make([]int, lb+1)
		dp[i][0] = i
	}
	for j := 0; j <= lb; j++ {
		dp[0][j] = j
	}
	for i := 1; i <= la; i++ {
		for j := 1; j <= lb; j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			dp[i][j] = min3(dp[i-1][j]+1, dp[i][j-1]+1, dp[i-1][j-1]+cost)
		}
	}
	return dp[la][lb]
}

func min3(a, b, c int) int {
	if a > b {
		a = b
	}
	if a > c {
		a = c
	}
	return a
}

func TestEditDistanceBasic(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "abc", 3},
		{"kitten", "sitting", 3},
		{"中文测试", "中文测验", 1},
		{"مرحبا", "مرحباً", 1},
	}
	for _, c := range cases {
		got := editDistanceBounded([]rune(c.a), []rune(c.b), -1)
		if got != c.want {
			t.Fatalf("dist(%q,%q)=%d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestEditDistanceBoundedMatchesRef(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	alphabet := []rune("abcde 中文")
	for trial := 0; trial < 2000; trial++ {
		la := rng.Intn(12)
		lb := rng.Intn(12)
		a := make([]rune, la)
		b := make([]rune, lb)
		for i := range a {
			a[i] = alphabet[rng.Intn(len(alphabet))]
		}
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		want := levRef(a, b)
		for _, maxDist := range []int{-1, 0, 1, 2, 3, 5, 20} {
			got := editDistanceBounded(a, b, maxDist)
			if maxDist < 0 || want <= maxDist {
				if got != want {
					t.Fatalf("dist(%q,%q,max=%d)=%d, want %d", string(a), string(b), maxDist, got, want)
				}
			} else if got <= maxDist {
				t.Fatalf("dist(%q,%q,max=%d)=%d should exceed max (true=%d)", string(a), string(b), maxDist, got, want)
			}
		}
	}
}

func TestSimilarity(t *testing.T) {
	cases := []struct{ d, m, want int }{
		{0, 10, 100},
		{1, 4, 75},
		{1, 3, 66},
		{5, 20, 75},
		{1, 7, 85},
	}
	for _, c := range cases {
		if got := similarity(c.d, c.m); got != c.want {
			t.Fatalf("similarity(%d,%d)=%d, want %d", c.d, c.m, got, c.want)
		}
	}
}
