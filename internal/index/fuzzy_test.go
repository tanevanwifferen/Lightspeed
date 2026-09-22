package index

import (
	"reflect"
	"testing"
)

func TestEditDistance(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"", "", 0}, {"abc", "", 3}, {"", "abc", 3}, {"kitten", "sitting", 3},
		{"servername", "servernmae", 1}, {"manager", "mangaer", 1}, {"ab", "ba", 1}, {"abc", "bca", 2}, {"same", "same", 0}, {"名前", "名字", 1},
	} {
		if got := editDistance(c.a, c.b); got != c.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := editDistance(c.b, c.a); got != c.want {
			t.Errorf("editDistance is not symmetric for %q, %q", c.a, c.b)
		}
	}
}

func TestJaccardAndTrigrams(t *testing.T) {
	if j := jaccard(trigrams("servername"), trigrams("servernmae")); j < 0.3 || j > 0.6 {
		t.Errorf("similar names: %v", j)
	}
	if j := jaccard(trigrams("abc"), trigrams("xyz")); j != 0 {
		t.Errorf("disjoint: %v", j)
	}
	if j := jaccard(trigrams("ab"), trigrams("ab")); j != 1 {
		t.Errorf("a name shorter than a trigram is its own trigram: %v", j)
	}
	if j := jaccard(trigrams(""), trigrams("ab")); j != 0 {
		t.Errorf("empty: %v", j)
	}
}

func TestFuzzyScoreThresholds(t *testing.T) {
	want := "servernmae"
	tri := trigrams(want)
	if s := fuzzyScore(want, tri, "servername"); s <= 0 || s > 1 {
		t.Errorf("a transposition must match, score %v", s)
	}
	if s := fuzzyScore(want, tri, "zebra"); s != 0 {
		t.Errorf("unrelated must not match, score %v", s)
	}
	near, far := fuzzyScore(want, tri, "servername"), fuzzyScore(want, tri, "servernames")
	if near <= 0 || far <= 0 || near < far {
		t.Errorf("closer must score at least as well: %v vs %v", near, far)
	}
}

func TestFuzzyScoreTransposition(t *testing.T) {
	// The commonest typo: two adjacent letters swapped in a short name.
	want := "mangaer"
	if s := fuzzyScore(want, trigrams(want), "manager"); s <= 0 {
		t.Errorf("mangaer must fuzzy-match manager, score %v", s)
	}
}

func TestNearestNames(t *testing.T) {
	names := []string{"Zebra", "serverName", "serverPort", "serverName", "servers", "x", "", "alpha", "beta", "gamma", "delta"}
	got := nearestNames("servernmae", names)
	// Only names that are actually near qualify: the three server* names, not
	// "Zebra" or "alpha" merely to make up five.
	if len(got) != 3 {
		t.Fatalf("got %d names: %v", len(got), got)
	}
	if got := nearestNames("qqqqqqqq", names); len(got) != 0 {
		t.Errorf("unrelated names were offered as nearest: %v", got)
	}
	if got[0] != "serverName" {
		t.Errorf("nearest first = %v", got)
	}
	seen := map[string]bool{}
	for _, n := range got {
		if seen[n] || n == "" {
			t.Errorf("duplicate or empty name in %v", got)
		}
		seen[n] = true
	}
	if again := nearestNames("servernmae", names); !reflect.DeepEqual(got, again) {
		t.Errorf("not deterministic: %v vs %v", got, again)
	}
}
