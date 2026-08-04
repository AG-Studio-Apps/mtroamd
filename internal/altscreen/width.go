package altscreen

// runeWidth returns the number of terminal cells a rune occupies: 0 for
// zero-width combining/format marks, 2 for East-Asian-Wide and wide emoji, 1
// otherwise. This is a compact, self-contained wcwidth (no external dependency)
// covering the ranges real TUIs emit; it deliberately treats "ambiguous-width"
// characters as 1, matching a UTF-8 xterm/tmux default.
func runeWidth(r rune) int {
	if r == 0 {
		return 0
	}
	if r < 0x20 || (r >= 0x7f && r < 0xa0) {
		return 0 // C0/C1 controls never reach putRune, but be safe
	}
	if r < 0x0300 {
		return 1 // fast path for Latin/ASCII
	}
	if inRanges(r, zeroWidthRanges) {
		return 0
	}
	if inRanges(r, wideRanges) {
		return 2
	}
	return 1
}

// inRanges does a binary search over a sorted, non-overlapping [lo,hi] table.
func inRanges(r rune, table [][2]rune) bool {
	lo, hi := 0, len(table)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		switch {
		case r < table[mid][0]:
			hi = mid - 1
		case r > table[mid][1]:
			lo = mid + 1
		default:
			return true
		}
	}
	return false
}

// zeroWidthRanges: combining marks, variation selectors, and zero-width
// formatting characters (ZWSP/ZWNJ/ZWJ, bidi controls, word joiner). Sorted.
var zeroWidthRanges = [][2]rune{
	{0x0300, 0x036F}, // combining diacritical marks
	{0x0483, 0x0489}, // Cyrillic combining
	{0x0591, 0x05BD}, // Hebrew points
	{0x05BF, 0x05BF},
	{0x05C1, 0x05C2},
	{0x05C4, 0x05C5},
	{0x05C7, 0x05C7},
	{0x0610, 0x061A}, // Arabic
	{0x064B, 0x065F},
	{0x0670, 0x0670},
	{0x06D6, 0x06DC},
	{0x06DF, 0x06E4},
	{0x06E7, 0x06E8},
	{0x06EA, 0x06ED},
	{0x0711, 0x0711},
	{0x0730, 0x074A},
	{0x07A6, 0x07B0},
	{0x07EB, 0x07F3},
	{0x0816, 0x0819},
	{0x081B, 0x0823},
	{0x0825, 0x0827},
	{0x0829, 0x082D},
	{0x0859, 0x085B},
	{0x08E3, 0x0902},
	{0x093A, 0x093A},
	{0x093C, 0x093C},
	{0x0941, 0x0948},
	{0x094D, 0x094D},
	{0x0951, 0x0957},
	{0x0962, 0x0963},
	{0x0E31, 0x0E31}, // Thai
	{0x0E34, 0x0E3A},
	{0x0E47, 0x0E4E},
	{0x1AB0, 0x1AFF},   // combining diacritical marks extended
	{0x1DC0, 0x1DFF},   // combining diacritical marks supplement
	{0x200B, 0x200F},   // ZWSP, ZWNJ, ZWJ, LRM, RLM
	{0x202A, 0x202E},   // bidi embedding/override
	{0x2060, 0x2064},   // word joiner, invisible operators
	{0x20D0, 0x20FF},   // combining marks for symbols
	{0xFE00, 0xFE0F},   // variation selectors
	{0xFE20, 0xFE2F},   // combining half marks
	{0xE0100, 0xE01EF}, // variation selectors supplement
}

// wideRanges: East-Asian-Wide / Fullwidth and wide emoji. Sorted.
var wideRanges = [][2]rune{
	{0x1100, 0x115F},   // Hangul Jamo
	{0x2329, 0x232A},   // angle brackets
	{0x2E80, 0x303E},   // CJK radicals … Kangxi, CJK symbols
	{0x3041, 0x33FF},   // Hiragana, Katakana, CJK symbols/compat
	{0x3400, 0x4DBF},   // CJK ext A
	{0x4E00, 0x9FFF},   // CJK unified ideographs
	{0xA000, 0xA4CF},   // Yi
	{0xA960, 0xA97F},   // Hangul Jamo extended-A
	{0xAC00, 0xD7A3},   // Hangul syllables
	{0xF900, 0xFAFF},   // CJK compatibility ideographs
	{0xFE10, 0xFE19},   // vertical forms
	{0xFE30, 0xFE6F},   // CJK compatibility forms, small form variants
	{0xFF00, 0xFF60},   // fullwidth forms
	{0xFFE0, 0xFFE6},   // fullwidth signs
	{0x1F004, 0x1F004}, // mahjong red dragon
	{0x1F0CF, 0x1F0CF}, // playing card black joker
	{0x1F18E, 0x1F18E}, // negative squared AB
	{0x1F191, 0x1F19A}, // squared CL … VS
	{0x1F200, 0x1F2FF}, // enclosed ideographic supplement
	{0x1F300, 0x1F64F}, // misc symbols & pictographs, emoticons
	{0x1F680, 0x1F6FF}, // transport & map symbols
	{0x1F900, 0x1F9FF}, // supplemental symbols & pictographs
	{0x1FA70, 0x1FAFF}, // symbols & pictographs extended-A
	{0x20000, 0x2FFFD}, // CJK ext B–F
	{0x30000, 0x3FFFD}, // CJK ext G
}
