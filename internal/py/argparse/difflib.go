package argparse

// The subset of CPython 3.15's difflib that ArgumentParser._check_value uses for its suggestion.

import "github.com/rexbrahh/make-better-decisions/internal/py/str"

// closeMatch returns difflib.get_close_matches(word, possibilities, 1)[0], or "" when nothing scores at
// least 0.6.
//
// py: Lib/difflib.py get_close_matches (Python 3.15)
func closeMatch(word string, possibilities []string) string {
	const cutoff = 0.6
	b := codePoints(word)
	best, bestScore := "", 0.0
	for _, x := range possibilities {
		a := codePoints(x)
		if realQuickRatio(a, b) < cutoff || quickRatio(a, b) < cutoff {
			continue
		}
		score := ratio(a, b)
		if score >= cutoff && (best == "" || score > bestScore || score == bestScore && x > best) {
			best, bestScore = x, score // heapq.nlargest(1) on (score, x)
		}
	}
	return best
}

func codePoints(s string) []rune {
	var runes []rune
	for s != "" {
		r, size := str.DecodeRune(s)
		runes, s = append(runes, r), s[size:]
	}
	return runes
}

// py: Lib/difflib.py _calculate_ratio (Python 3.15)
func calculateRatio(matches, length int) float64 {
	if length == 0 {
		return 1.0
	}
	return 2.0 * float64(matches) / float64(length)
}

// py: Lib/difflib.py SequenceMatcher.real_quick_ratio (Python 3.15)
func realQuickRatio(a, b []rune) float64 {
	return calculateRatio(min(len(a), len(b)), len(a)+len(b))
}

// py: Lib/difflib.py SequenceMatcher.quick_ratio (Python 3.15)
func quickRatio(a, b []rune) float64 {
	avail := map[rune]int{}
	for _, r := range b {
		avail[r]++
	}
	matches := 0
	for _, r := range a {
		if avail[r] > 0 {
			matches++
		}
		avail[r]--
	}
	return calculateRatio(matches, len(a)+len(b))
}

// ratio returns SequenceMatcher(None, a, b).ratio(), with the autojunk heuristic.
//
// py: Lib/difflib.py SequenceMatcher.ratio, get_matching_blocks, find_longest_match (Python 3.15)
func ratio(a, b []rune) float64 {
	b2j := map[rune][]int{}
	for j, r := range b {
		b2j[r] = append(b2j[r], j)
	}
	if n := len(b); n >= 200 {
		for r, indices := range b2j {
			if len(indices) > n/100+1 {
				delete(b2j, r) // a popular element
			}
		}
	}
	longest := func(alo, ahi, blo, bhi int) (int, int, int) {
		besti, bestj, bestsize := alo, blo, 0
		j2len := map[int]int{}
		for i := alo; i < ahi; i++ {
			next := map[int]int{}
			for _, j := range b2j[a[i]] {
				if j < blo {
					continue
				}
				if j >= bhi {
					break
				}
				k := j2len[j-1] + 1
				next[j] = k
				if k > bestsize {
					besti, bestj, bestsize = i-k+1, j-k+1, k
				}
			}
			j2len = next
		}
		// The junk set is empty, so only the extension over popular elements applies.
		for besti > alo && bestj > blo && a[besti-1] == b[bestj-1] {
			besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
		}
		for besti+bestsize < ahi && bestj+bestsize < bhi && a[besti+bestsize] == b[bestj+bestsize] {
			bestsize++
		}
		return besti, bestj, bestsize
	}
	matches := 0
	queue := [][4]int{{0, len(a), 0, len(b)}}
	for len(queue) > 0 {
		q := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		alo, ahi, blo, bhi := q[0], q[1], q[2], q[3]
		i, j, k := longest(alo, ahi, blo, bhi)
		if k == 0 {
			continue
		}
		matches += k
		if alo < i && blo < j {
			queue = append(queue, [4]int{alo, i, blo, j})
		}
		if i+k < ahi && j+k < bhi {
			queue = append(queue, [4]int{i + k, ahi, j + k, bhi})
		}
	}
	return calculateRatio(matches, len(a)+len(b))
}
