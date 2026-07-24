package skill

import (
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// Similarity thresholds for the skill-factory dedup gates. Both operate on
// TokenJaccard over the same tokenization, but serve different decisions:
const (
	// LibraryDedupThreshold flags an existing library skill as "similar" to
	// a candidate. Deliberately low: a flagged skill is an advisory hint
	// shown to the operator (and an approve blocker unless forced), so
	// false positives cost a --force, while false negatives cost skill bloat.
	LibraryDedupThreshold = 0.5
	// CandidateMergeThreshold merges a newly reported candidate into an
	// existing pending candidate (incrementing Occurrences) instead of
	// creating a new entry. Deliberately high: a wrong merge silently loses
	// a distinct pattern, so only near-identical rewordings collapse.
	CandidateMergeThreshold = 0.8
)

// LibrarySkill is a skill discovered in one of the skill source directories
// (bundled catalog or skills.extra_dirs), tagged with its role scope and path.
type LibrarySkill struct {
	Content
	// Role is the scope directory the skill was found under ("worker",
	// "planner", "share", ...).
	Role string
	// Path is the absolute path of the SKILL.md file.
	Path string
}

// Ref returns the "<role>/<name>" reference used to point operators at an
// existing skill.
func (l LibrarySkill) Ref() string {
	name := l.Name
	if name == "" {
		name = l.ID
	}
	return path.Join(l.Role, name)
}

// ListAllSkills walks every skill source directory (each laid out as
// <dir>/<role>/<name>/SKILL.md) and returns all parseable skills across all
// role scopes. Duplicates (same role/name in a later directory) are skipped:
// earlier directories take precedence, matching ResolveSearchDirs ordering.
// Parse and I/O errors are logged as warnings and the entry is skipped; the
// listing is best-effort because it feeds advisory dedup, not dispatch.
func ListAllSkills(skillsDirs []string, logger *slog.Logger) []LibrarySkill {
	if logger == nil {
		logger = slog.Default()
	}
	seen := make(map[string]struct{})
	var out []LibrarySkill

	for _, base := range skillsDirs {
		fsys := os.DirFS(base)
		walkErr := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				// Missing/unreadable subtree: skip, best-effort listing.
				return nil //nolint:nilerr // best-effort walk; unreadable entries are skipped by design
			}
			if d.IsDir() || d.Name() != "SKILL.md" {
				return nil
			}
			dir := path.Dir(p) // "<role>/<name>"
			role := path.Dir(dir)
			name := path.Base(dir)
			if role == "." || strings.Contains(role, "/") {
				// Not the <role>/<name>/SKILL.md layout; ignore.
				return nil
			}
			key := role + "/" + name
			if _, dup := seen[key]; dup {
				return nil
			}
			data, readErr := fs.ReadFile(fsys, p)
			if readErr != nil {
				logger.Warn("ListAllSkills: failed to read SKILL.md", "path", filepath.Join(base, p), "error", readErr)
				return nil
			}
			meta, body, parseErr := parseFrontmatter(string(data))
			if parseErr != nil {
				logger.Warn("ListAllSkills: failed to parse frontmatter", "path", filepath.Join(base, p), "error", parseErr)
				return nil
			}
			meta.ID = name
			if meta.Name == "" {
				meta.Name = name
			}
			seen[key] = struct{}{}
			out = append(out, LibrarySkill{
				Content: Content{Metadata: meta, Body: body},
				Role:    role,
				Path:    filepath.Join(base, p),
			})
			return nil
		})
		if walkErr != nil {
			logger.Warn("ListAllSkills: walk failed", "dir", base, "error", walkErr)
		}
	}
	return out
}

// FindSimilarSkills returns the "<role>/<name>" references of library skills
// whose description+body similarity to content is at or above threshold.
// Results are ordered by descending similarity (ties by ref) so the strongest
// duplicate is presented first.
func FindSimilarSkills(content string, library []LibrarySkill, threshold float64) []string {
	type scored struct {
		ref   string
		score float64
	}
	candidateTokens := tokenizeForSimilarity(content)
	var matches []scored
	for _, s := range library {
		score := jaccard(candidateTokens, tokenizeForSimilarity(s.Description+"\n"+s.Body))
		if score >= threshold {
			matches = append(matches, scored{ref: s.Ref(), score: score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].ref < matches[j].ref
	})
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.ref)
	}
	return out
}

// TokenJaccard computes the Jaccard similarity of the token sets of a and b
// in [0, 1]. Latin-script words are lowercased whole tokens; CJK runs are
// decomposed into character bigrams so Japanese prose compares meaningfully
// without a morphological analyzer.
func TokenJaccard(a, b string) float64 {
	return jaccard(tokenizeForSimilarity(a), tokenizeForSimilarity(b))
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	small, large := a, b
	if len(small) > len(large) {
		small, large = large, small
	}
	inter := 0
	for tok := range small {
		if _, ok := large[tok]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// tokenizeForSimilarity splits s into a token set: contiguous letter/digit
// runs are tokens; within a run, CJK characters are emitted as character
// bigrams (and lone CJK chars as unigrams) while non-CJK segments are emitted
// as whole lowercased words.
func tokenizeForSimilarity(s string) map[string]struct{} {
	tokens := make(map[string]struct{})
	var word []rune
	flushWord := func() {
		if len(word) > 0 {
			tokens[strings.ToLower(string(word))] = struct{}{}
			word = word[:0]
		}
	}
	var prevCJK rune
	cjkRunLen := 0
	flushCJK := func() {
		// A lone CJK char has produced no bigram; keep it as a unigram so it
		// still participates in the token set. Longer runs are fully covered
		// by their bigrams.
		if cjkRunLen == 1 {
			tokens[string(prevCJK)] = struct{}{}
		}
		prevCJK = 0
		cjkRunLen = 0
	}
	for _, r := range s {
		switch {
		case isCJK(r):
			flushWord()
			if prevCJK != 0 {
				tokens[string([]rune{prevCJK, r})] = struct{}{}
			}
			prevCJK = r
			cjkRunLen++
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			word = append(word, r)
		default:
			flushCJK()
			flushWord()
		}
	}
	flushCJK()
	flushWord()
	return tokens
}

// isCJK reports whether r belongs to a script where word boundaries are not
// whitespace-delimited (Han, Hiragana, Katakana).
func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana)
}
