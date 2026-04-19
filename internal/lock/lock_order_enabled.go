//go:build lockorder

package lock

import (
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// lockOrderMode controls the behaviour of the debug order checker.
type lockOrderMode uint8

const (
	lockOrderOff   lockOrderMode = iota // tracking disabled
	lockOrderWarn                       // log violations
	lockOrderPanic                      // panic on violation (for tests)
)

// heldLock records a single lock acquisition for a goroutine.
type heldLock struct {
	key   string
	level int
}

// debugOrderChecker tracks per-goroutine lock acquisitions and verifies
// that locks are acquired in the canonical order:
//
//	queue:* (level 1) → state:* (level 2) → result:* (level 3)
//
// It is compiled in only when the "lockorder" build tag is set.
// The runtime behaviour is controlled by the MAESTRO_LOCKORDER environment
// variable: off (default) | warn | panic.
type debugOrderChecker struct {
	mu   sync.Mutex
	held map[uint64][]heldLock // goroutine ID → currently held locks
	mode lockOrderMode
}

func newOrderChecker() orderChecker {
	return &debugOrderChecker{
		held: make(map[uint64][]heldLock),
		mode: parseLockOrderMode(os.Getenv("MAESTRO_LOCKORDER")),
	}
}

func parseLockOrderMode(v string) lockOrderMode {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "warn", "1", "true":
		return lockOrderWarn
	case "panic", "strict", "2":
		return lockOrderPanic
	default:
		return lockOrderOff
	}
}

// levelForKey returns the canonical lock level for a key prefix.
// Keys that do not match a known prefix return (0, false) and are
// excluded from order checking.
func levelForKey(key string) (int, bool) {
	switch {
	case strings.HasPrefix(key, "queue:"):
		return 1, true
	case strings.HasPrefix(key, "state:"):
		return 2, true
	case strings.HasPrefix(key, "result:"):
		return 3, true
	default:
		return 0, false
	}
}

func (c *debugOrderChecker) BeforeLock(key string) {
	if c.mode == lockOrderOff {
		return
	}
	level, ok := levelForKey(key)
	if !ok {
		return
	}
	gid := currentGID()

	c.mu.Lock()
	held := c.held[gid]
	maxLevel, maxKey := 0, ""
	for _, h := range held {
		if h.level > maxLevel {
			maxLevel, maxKey = h.level, h.key
		}
	}
	c.mu.Unlock()

	if maxLevel > level {
		msg := fmt.Sprintf(
			`lock order violation: acquiring %q (level %d) while holding %q (level %d); expected queue:* → state:* → result:*`,
			key, level, maxKey, maxLevel,
		)
		c.violate(msg)
	}
}

func (c *debugOrderChecker) AfterLock(key string) {
	if c.mode == lockOrderOff {
		return
	}
	level, ok := levelForKey(key)
	if !ok {
		return
	}
	gid := currentGID()

	c.mu.Lock()
	c.held[gid] = append(c.held[gid], heldLock{key: key, level: level})
	c.mu.Unlock()
}

func (c *debugOrderChecker) BeforeUnlock(key string) {
	if c.mode == lockOrderOff {
		return
	}
	if _, ok := levelForKey(key); !ok {
		return
	}
	gid := currentGID()

	c.mu.Lock()
	defer c.mu.Unlock()

	locks := c.held[gid]
	for i := len(locks) - 1; i >= 0; i-- {
		if locks[i].key == key {
			locks = append(locks[:i], locks[i+1:]...)
			break
		}
	}
	if len(locks) == 0 {
		delete(c.held, gid)
	} else {
		c.held[gid] = locks
	}
}

// violate handles lock-order violations. This function is only compiled when
// the "lockorder" build tag is set (see lock_order_disabled.go for the
// production no-op). In warn mode it logs; in panic mode (used by tests) it
// panics to surface ordering bugs early.
func (c *debugOrderChecker) violate(msg string) {
	if c.mode == lockOrderWarn {
		slog.Warn("lock order violation", "detail", msg)
		return
	}
	panic(msg)
}

// currentGID extracts the goroutine ID from a runtime.Stack snapshot.
//
// ⚠ Go バージョン依存リスク:
//
// runtime.Stack() の出力形式 "goroutine <id> [<status>]:\n..." は Go の公式仕様
// (言語仕様・標準ライブラリのドキュメント) で保証されていない実装詳細である。
// Go 1.0 以降 Go 1.x 系では事実上安定しているが、将来のバージョン（特にメジャー
// バージョン変更時）に形式が変更される可能性がある。
//
// 変更時の影響: goroutine ID の取得に失敗し 0 が返る → lock ordering 検出が
// 事実上無効化される（ただし lockorder ビルドタグ有効時のデバッグ機能のみに
// 影響し、本番動作には影響しない）。
//
// 代替手段の候補:
//   - runtime.GoID(): Go チームで提案中だが未採用 (proposal #69321 等)
//   - goroutine-local storage ライブラリ (github.com/petermattis/goid 等)
//   - sync.Mutex のラッパーで caller の識別に別手法を使用
//
// 現時点での許容判断:
//   - Go 1.x 系では形式が安定しており実績がある
//   - lockorder ビルドタグによるデバッグ専用コードであり本番には含まれない
//   - パース失敗時は 0 を返して安全に縮退する (lock order 検出をスキップ)
//   - Go バージョンアップ時のテスト (go test -tags lockorder) で形式変更を検出可能
func currentGID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false) // "goroutine 123 [running]:\n..."
	s := strings.TrimPrefix(string(buf[:n]), "goroutine ")
	i := strings.IndexByte(s, ' ')
	if i <= 0 {
		return 0
	}
	id, _ := strconv.ParseUint(s[:i], 10, 64)
	return id
}
