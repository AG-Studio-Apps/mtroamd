package release

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// tagPattern is the only shape we accept for a release tag. Anchored
// at both ends, no shell metacharacters, no slashes or path-traversal
// sequences. Optional pre-release suffix matches things like "-rc1",
// "-alpha.2", "-beta". Build metadata (`+meta`) is deliberately
// rejected — we don't use it, and accepting `+` would force us to
// reason about URL-encoding behaviour in fetcher.AssetURL.
var tagPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$`)

// MaxTagLength caps the total length of an accepted tag. 64 is well
// above any plausible semver string and far below any URL-length
// problem; rejecting earlier means a path-traversal payload can't
// even reach the regex engine.
const MaxTagLength = 64

// ValidateTag returns nil iff `tag` is shaped like a release tag we
// would publish. Used at the boundary in `mtroamd update` /
// `mtroam update` to reject `--tag` values (or surprising responses
// from the GitHub API) before they're interpolated into asset URLs.
//
// Closes one of the LOW-severity audit gaps from the 2026-05-11
// review: before this guard, a tag like "v1.0/../../etc/passwd"
// would have been passed straight into fmt.Sprintf in AssetURL.
func ValidateTag(tag string) error {
	if tag == "" {
		return fmt.Errorf("empty tag")
	}
	if len(tag) > MaxTagLength {
		return fmt.Errorf("tag exceeds %d characters", MaxTagLength)
	}
	if !tagPattern.MatchString(tag) {
		return fmt.Errorf("tag %q does not match vMAJOR.MINOR.PATCH[-suffix]", tag)
	}
	return nil
}

// describeTrailer matches the suffix `git describe --tags` appends past
// a tag when the build is ahead of it: "-<commits>-g<shorthash>".
// Stripped (along with a trailing "-dirty") BEFORE we read the
// pre-release identifier, so a post-tag dev build keeps the SAME
// pre-release as its base tag (e.g. "v1.6.2-rc1-3-gabc" → pre "rc1")
// while a genuine release tag's "-rc1" is preserved. Anchored at end.
// (hash class is case-insensitive defensively; git emits lowercase.)
var describeTrailer = regexp.MustCompile(`-[0-9]+-g[0-9a-fA-F]+$`)

// splitVersion decomposes a version string into its MAJOR.MINOR.PATCH
// triplet and its pre-release identifier ("" for a final release),
// after removing git-describe trailers ("-dirty", "-<n>-g<hash>").
// ok is false if the triplet doesn't parse.
//
//	"v1.6.2-rc1"            → ([1 6 2], "rc1")
//	"v1.6.2"               → ([1 6 2], "")
//	"v1.6.2-rc1-3-gabc123" → ([1 6 2], "rc1")   // dev build off rc1
//	"v1.6.2-3-gabc123"     → ([1 6 2], "")       // dev build off final
//	"v1.6.2-rc1-dirty"     → ([1 6 2], "rc1")
func splitVersion(s string) (triplet [3]int, prerelease string, ok bool) {
	v := strings.TrimPrefix(s, "v")
	v = strings.TrimSuffix(v, "-dirty")
	v = describeTrailer.ReplaceAllString(v, "")
	base := v
	if i := strings.Index(v, "-"); i != -1 {
		base = v[:i]
		prerelease = v[i+1:]
	}
	parts := strings.SplitN(base, ".", 4)
	if len(parts) < 3 {
		return [3]int{}, "", false
	}
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return [3]int{}, "", false
		}
		triplet[i] = n
	}
	return triplet, prerelease, true
}

// VersionsMatch returns true if `current` and `target` refer to the
// same version — same triplet AND same pre-release. The build stamp is
// whatever `git describe --tags --dirty --always` printed (e.g.
// "v1.6.2", "v1.6.2-rc1", "v1.6.2-rc1-3-gabc1234", "v1.6.2-dirty");
// the target is typically a clean tag. git-describe trailers are
// stripped first, so a dirty / post-tag build still matches its base
// tag, but a real pre-release is distinguished: v1.6.2-rc1 ≠ v1.6.2-rc2
// and v1.6.2-rc1 ≠ v1.6.2 (so an rc can be replaced by the next rc or
// by the final release — the bug this fixes).
func VersionsMatch(current, target string) bool {
	ct, cp, ok1 := splitVersion(current)
	tt, tp, ok2 := splitVersion(target)
	if !ok1 || !ok2 {
		return BaseTag(current) == BaseTag(target)
	}
	return ct == tt && cp == tp
}

// CompareSemver returns (-1, 0, +1) comparing a vs b, or (0, false) if
// either side doesn't parse. Triplets compare first; on a tie, semver
// pre-release precedence applies: a final release outranks any
// pre-release of the same triplet, and two pre-releases compare
// naturally (so rc2 < rc10, not lexical). Drives anti-rollback —
// rc1→rc2 and rc→final are upgrades; rc2→rc1 a downgrade.
func CompareSemver(a, b string) (int, bool) {
	at, ap, ok1 := splitVersion(a)
	bt, bp, ok2 := splitVersion(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		switch {
		case at[i] < bt[i]:
			return -1, true
		case at[i] > bt[i]:
			return +1, true
		}
	}
	return comparePrerelease(ap, bp), true
}

// comparePrerelease orders two pre-release identifiers of an otherwise
// equal triplet. An empty identifier (a final release) ranks ABOVE any
// pre-release (semver §11). Two non-empty identifiers compare
// naturally so numeric runs order numerically (rc2 < rc10).
func comparePrerelease(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return +1 // final > pre-release
	case b == "":
		return -1
	default:
		return naturalCompare(a, b)
	}
}

// naturalCompare compares two strings treating maximal digit runs as
// numbers ("rc2" < "rc10"); non-digit runs compare bytewise. Numeric
// runs are compared as strings (leading-zero-trimmed length, then
// lexical) so arbitrarily long runs can't overflow.
func naturalCompare(a, b string) int {
	for len(a) > 0 && len(b) > 0 {
		aDigit := a[0] >= '0' && a[0] <= '9'
		bDigit := b[0] >= '0' && b[0] <= '9'
		if aDigit && bDigit {
			ar, arest := digitRun(a)
			br, brest := digitRun(b)
			if c := compareNumericRun(ar, br); c != 0 {
				return c
			}
			a, b = arest, brest
			continue
		}
		if a[0] != b[0] {
			if a[0] < b[0] {
				return -1
			}
			return +1
		}
		a, b = a[1:], b[1:]
	}
	switch {
	case len(a) == len(b):
		return 0
	case len(a) < len(b):
		return -1
	default:
		return +1
	}
}

// digitRun splits a leading run of ASCII digits off s.
func digitRun(s string) (run, rest string) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i], s[i:]
}

// compareNumericRun orders two ASCII-digit strings as numbers without
// parsing (overflow-proof): strip leading zeros, then the shorter is
// the smaller magnitude, ties broken lexically.
func compareNumericRun(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	switch {
	case len(a) != len(b):
		if len(a) < len(b) {
			return -1
		}
		return +1
	case a < b:
		return -1
	case a > b:
		return +1
	default:
		return 0
	}
}

// ParseSemver extracts MAJOR.MINOR.PATCH ints from a version string of
// the form "vMAJOR.MINOR.PATCH" (optional "v" prefix and "-suffix").
func ParseSemver(s string) ([3]int, bool) {
	t, _, ok := splitVersion(s)
	return t, ok
}

// BaseTag strips a leading "v" and a trailing "-anything" so we get
// the bare "X.Y.Z" form. Used as the VersionsMatch fallback for
// strings that don't parse as a triplet.
func BaseTag(s string) string {
	s = strings.TrimPrefix(s, "v")
	if i := strings.Index(s, "-"); i != -1 {
		s = s[:i]
	}
	return s
}
