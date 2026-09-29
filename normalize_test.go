package tmem

import "testing"

func TestNormalizeCollapsesWhitespace(t *testing.T) {
	in := "  hello \t world\u00a0\u00a0foo　　bar\r\nbaz  "
	got, err := Normalize(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "hello world foo bar baz"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeKeepsCaseAndInnerSingleSpace(t *testing.T) {
	got, err := Normalize("AbC d")
	if err != nil || got != "AbC d" {
		t.Fatalf("got %q, err %v", got, err)
	}
}

func TestNormalizeRejectsEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\u00a0　\t\n"} {
		if _, err := Normalize(in); err != ErrInvalidText {
			t.Fatalf("input %q: got err %v, want ErrInvalidText", in, err)
		}
	}
}

func TestNormalizeRejectsInvalidUTF8(t *testing.T) {
	if _, err := Normalize("abc\xff\xfe"); err != ErrInvalidText {
		t.Fatalf("got err %v, want ErrInvalidText", err)
	}
}

func TestNormalizeRejectsTooLong(t *testing.T) {
	long := make([]rune, MaxTextRunes+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := Normalize(string(long)); err != ErrInvalidText {
		t.Fatalf("got err %v, want ErrInvalidText", err)
	}
	// 恰好 1000 个码点应通过
	ok := make([]rune, MaxTextRunes)
	for i := range ok {
		ok[i] = '中'
	}
	if _, err := Normalize(string(ok)); err != nil {
		t.Fatalf("1000 runes should pass, got %v", err)
	}
}
