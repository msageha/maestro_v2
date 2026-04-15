package model

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const maxRawErrorLength = 1024

// FailureFingerprint はエラーの正規化ハッシュと分類を保持する。
type FailureFingerprint struct {
	Hash      string
	Category  string // build, lint, test, typecheck, runtime
	RawError  string
	CreatedAt time.Time
}

// ConsecutiveFailureTracker は連続する同一エラーを検知するトラッカーである。
type ConsecutiveFailureTracker struct {
	mu           sync.Mutex
	fingerprints []FailureFingerprint
	maxHistory   int
}

var (
	// 行番号パターン: `:123:` や `line 123`
	reLineNumber = regexp.MustCompile(`(?::(\d+):)|(?:\bline\s+\d+\b)`)
	// ISO8601 タイムスタンプ
	reISO8601 = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	// Unix タイムスタンプ（10桁または13桁の数値）
	reUnixTimestamp = regexp.MustCompile(`\b\d{10,13}\b`)
	// 絶対パス
	reAbsPath = regexp.MustCompile(`/[\w./\-]+`)
	// メモリアドレス
	reMemAddr = regexp.MustCompile(`0x[0-9a-fA-F]+`)
	// 連続空白
	reMultiSpace = regexp.MustCompile(`\s+`)
)

// NormalizeError はエラー文字列を正規化する。
// 行番号、タイムスタンプ、絶対パス、メモリアドレスを除去・抽象化し、
// 連続空白を正規化して小文字化する。
func NormalizeError(raw string) string {
	s := raw

	// 1. タイムスタンプ除去（行番号除去より先に実行。ISO8601の `:30:` が行番号パターンに誤マッチするのを防ぐ）
	s = reISO8601.ReplaceAllString(s, "")
	s = reUnixTimestamp.ReplaceAllString(s, "")

	// 2. 行番号除去
	s = reLineNumber.ReplaceAllString(s, "")

	// 3. パス抽象化: 絶対パスをベース名に置換
	s = reAbsPath.ReplaceAllStringFunc(s, func(match string) string {
		base := filepath.Base(match)
		if base == "." || base == "/" {
			return ""
		}
		return base
	})

	// 4. メモリアドレス除去
	s = reMemAddr.ReplaceAllString(s, "ADDR")

	// 5. 連続空白正規化
	s = reMultiSpace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)

	// 6. 小文字化
	s = strings.ToLower(s)

	return s
}

// ComputeFingerprint は正規化済みエラー文字列とカテゴリからフィンガープリントを生成する。
// now にはフィンガープリントの作成時刻を渡す。テスト時には固定時刻を使用できる。
func ComputeFingerprint(normalized string, category string, now time.Time) FailureFingerprint {
	h := sha256.New()
	h.Write([]byte(category))
	h.Write([]byte{0}) // セパレータ
	h.Write([]byte(normalized))
	hash := fmt.Sprintf("%x", h.Sum(nil))

	rawError := normalized
	if len(rawError) > maxRawErrorLength {
		rawError = rawError[:maxRawErrorLength]
	}

	return FailureFingerprint{
		Hash:      hash,
		Category:  category,
		RawError:  rawError,
		CreatedAt: now,
	}
}

// NewConsecutiveFailureTracker は新しいトラッカーを生成する。
func NewConsecutiveFailureTracker(maxHistory int) *ConsecutiveFailureTracker {
	if maxHistory <= 0 {
		maxHistory = 10
	}
	return &ConsecutiveFailureTracker{
		fingerprints: make([]FailureFingerprint, 0, maxHistory),
		maxHistory:   maxHistory,
	}
}

// Record はフィンガープリントを記録する。
func (t *ConsecutiveFailureTracker) Record(fp FailureFingerprint) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.fingerprints = append(t.fingerprints, fp)
	if len(t.fingerprints) > t.maxHistory {
		t.fingerprints = t.fingerprints[len(t.fingerprints)-t.maxHistory:]
	}
}

// IsConsecutiveDuplicate は直近 threshold 件が全て同一ハッシュかどうかを返す。
func (t *ConsecutiveFailureTracker) IsConsecutiveDuplicate(threshold int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if threshold <= 0 || len(t.fingerprints) < threshold {
		return false
	}

	last := t.fingerprints[len(t.fingerprints)-1].Hash
	for i := len(t.fingerprints) - threshold; i < len(t.fingerprints); i++ {
		if t.fingerprints[i].Hash != last {
			return false
		}
	}
	return true
}

// LastFingerprint は最後に記録されたフィンガープリントを返す。
// 記録がない場合は nil を返す。
func (t *ConsecutiveFailureTracker) LastFingerprint() *FailureFingerprint {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.fingerprints) == 0 {
		return nil
	}
	fp := t.fingerprints[len(t.fingerprints)-1]
	return &fp
}

// Reset は記録をクリアする。
func (t *ConsecutiveFailureTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.fingerprints = t.fingerprints[:0]
}
