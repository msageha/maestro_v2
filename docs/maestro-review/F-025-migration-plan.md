# F-025 Migration Plan: Worker Policy Hook の Go 化

- 作成日: 2026-04-27
- 対象 Finding: F-025 (Medium, `internal/agent/policy_checker.go`)
- ステータス: **Step 1-6 着地、Step 7 は config-validate でゲート (experimental)**。Step 8 (bash ロジック削除) は Go policy の full corpus parity 確認後に実施

## 背景

`policy_checker.go` の `hookScript` 文字列 (約 580 行) が PreToolUse 安全ポリシーの Single Source of Truth (SSOT)。Worker 起動時に `.maestro/hooks/worker-policy.sh` に書き出され、Claude Code の hook として実行される。

レビュー指摘 (F-025):

- 600 行超の正規表現ベース bash hook は Go 側 SSOT (`internal/validate/policy.go` — 未存在) と二重保守になりうる
- shell escaping のバグが安全網の穴になりうる
- 推奨: Tier1/Tier2 判定を Go に集約し、bash hook を Go バイナリのラッパに痩せさせる

## 判断: 段階的移行 (defer with documented risk)

- **即時 Go 化はリスクが高い**: bash と Go の意味差分が安全機構に直接影響。117 件の既存テストが現在の挙動を厳密に固定している
- **長期的には Go 化の価値あり**: 型付き JSON parse、構造化 decision、shell escaping 排除、cross-platform 対応
- **codex 相談 (2026-04-27)**: "won’t fix" より "defer with documented risk" を推奨。bash 外部ファイル化と migration plan の文書化を最初に行うのが ROI 最大

## 段階的移行計画

### Step 1: bash 外部ファイル化 ✅ 完了 (2026-04-27)

**変更内容**:

- `internal/agent/worker_policy_hook.sh` を新設 (588 行)
- `policy_checker.go` で `//go:embed worker_policy_hook.sh` を介して読み込み
- `hookScript = strings.TrimSpace(hookScriptRaw) + "\n"` で本体ロジックを維持

**結果**:

- `policy_checker.go`: 710 → 133 行
- shell-lint / shellcheck / 実行ファイルとして直接編集可能に
- `go test ./internal/agent/...` 全テスト green を確認 (機能不変)

**注**: 「バイト等価」ではなく「セマンティクス保存 + Step 6 の `shadow_policy_check` フック追加 + F-027 deny reason 文言拡充 + `project_root` の hoist」を含む。117 件の bash hook tests は構造照合 (substring / 行構造) ベースのためすべて green を維持できた。次回以降のレビューで「バイト等価」と誤読しないよう、ここに実態を明記する。

**ROI**: 最小工数で可読性大幅改善。Go 化の前提条件 (bash がレビュー可能・editor で扱いやすい状態) を整える。

### Step 2: 既存 117 テストから golden corpus 作成 ✅ 完了 (2026-04-27)

**目的**: bash と Go の挙動 parity を after-the-fact で検証できる回帰防止網を作る。

**方針**:

- `internal/validate/policy_corpus.json` を新設
- 現在 Go に翻訳済みの rule set について「入力 / 期待 allow / 期待 deny reason」を固定
- `internal/agent/policy_parity_test.go` は hardcoded matrix を廃止し、この corpus を読み込む
- parity test は Allow/Deny だけでなく deny reason も byte-for-byte 比較する
- 既存の 117 bash hook tests はそのまま残し、legacy bash の regression net として維持

### Step 3: `internal/validate/policy.go` に Go 純粋関数を実装 (✅ スケルトン着地、ルール拡張は次 sprint)

**進捗 (2026-04-27)**:

- `internal/validate/policy.go` を新設 — `HookInput` / `HookDecision` 型 + `CheckWorkerPolicy` 関数のスケルトン着地
- 実装ルール:
  - 非 Bash ツール (Read など) → Allow
  - Write / Edit → Allow (scaffold; 後続 sprint で拡張)
  - Bash 空コマンド → Allow
  - **C1**: backtick command substitution → Deny (`\``)
  - **C1**: ANSI-C quoting (`$'...'`) → Deny (anchor: 行頭 / `\s;|&({"'=`)
  - **H1-PS**: process substitution (`<(...)` or `>(...)`) → Deny
  - **D001**: `rm -rf` / `--recursive --force` + system target (`/`, `~`, `/Users`, `/home`, `/root`, `/opt`) → Deny
  - **D004**: `git reset --hard` → Deny
  - **Worker git push** (全形式、`--force-with-lease` 含む、F-027): → Deny
  - **D005**: `sudo` / `su` (privilege escalation、shell separator anchor) → Deny
  - **D006**: `kill` / `killall` / `pkill` (process destruction) → Deny
  - **D008**: `(curl|wget) ... | (ba)?sh` (remote code execution via pipe) → Deny
- `internal/validate/policy_test.go` に 28 ケースの allow/deny マトリクス
- `shellSeparatorAnchor = (?:^|;|\||&&)` 共通定数を導入し D005/D006 等の anchor を統一
- **重要**: Go 関数は CheckWorkerPolicy をエクスポートしただけで、Worker 起動経路では呼ばれていない (bash hook のみが enforcing)。Step 4 (parity test) と Step 7 (feature flag 切替) が landed されるまで、この Go 関数は observability/parity 用の dead code として確保
- 学習: bash の `grep -E` には `\b` がないが、Go の `regexp` で `\b` を入れると `-rf` のような連続単語境界ケースで取り逃がす。bash 翻訳時は `\b` を慎重に避ける必要あり (policy.go の rmRecursiveFlag/rmForceFlag コメント参照)

**API 案**:

```go
package validate

type HookInput struct {
    ToolName    string
    Command     string
    FilePath    string
    RunOnMain   bool
    ProjectRoot string
}

type HookDecision struct {
    Allow bool
    Reason string  // empty when Allow=true
}

func CheckWorkerPolicy(in HookInput) HookDecision
```

**注意点**:

- `regexp` ベースの実装で bash の `grep -qE` と互換性を保つ (`(?i)`、word boundary `\b` の挙動差に注意)
- `RUN_ON_MAIN` flag は呼び出し元 (CLI / hook ラッパ) で解決し、`HookInput.RunOnMain` で渡す
- 単体テストは Step 2 の corpus を直接使う

### Step 4: bash と Go の parity test (✅ 足場着地、corpus 拡張は逐次)

**進捗 (2026-04-27)**:

- `internal/agent/policy_parity_test.go` を新設 (`TestPolicyParity_BashVsGo`)
- corpus 18 エントリ (Allow 3 + Deny 15) で現状 Step 3 が翻訳済みのルール (C1 backtick / C1 ANSI-C / H1-PS / D001 / D004 / Worker git push / D005 sudo / D006 kill / D006 pkill / D008 curl|sh) について、bash hook と `validate.CheckWorkerPolicy` の Allow/Deny が完全一致することを確認
- `requireJq` で jq 未インストール時は skip、CI が jq を提供すれば常時実行
- **Allow/Deny 一致のみ検証** — deny reason 文字列の byte-for-byte 比較は corpus が成熟する Step 6 で導入。スコープと理由を test header godoc に明記
- 規約: Step 3 で新ルールを翻訳するたび、対応する Allow + Deny ケースを必ず本 corpus に追加 (test 内コメントに明記)

### Step 5: `maestro hook policy-check` CLI サブコマンド追加 ✅ 完了 (2026-04-27)

- stdin から Claude Code PreToolUse hook input JSON (`tool_name`, `tool_input.command`, `tool_input.file_path`) を受け取り、stdout に decision JSON を出す
- `--run-on-main` / `--project-root` を受け取り、将来の RUN_ON_MAIN / path confinement 翻訳に備える
- deny 時は top-level `{allow:false, reason}` に加え、Claude hook 互換の `hookSpecificOutput.permissionDecision=deny` も出力する
- bash 経由を保ったまま、`internal/validate.CheckWorkerPolicy` を `maestro` バイナリから呼べるようにした
- `cmd_hook_test.go` に allow / deny / invalid JSON の CLI 単体テストを追加

### Step 6: Shadow mode ✅ 完了 (2026-04-27)

- bash hook 内に `shadow_policy_check` を追加
- `MAESTRO_POLICY_SHADOW=1` または config の `agents.workers.policy_hook_implementation: "shadow"` で有効化
- bash が authoritative enforcement のまま、`maestro hook policy-check` の結果を比較
- Allow/Deny または deny reason が異なる場合のみ stderr に `maestro_policy_shadow_divergence ...` を出力
- `maestro hook policy-check` 実行失敗や malformed output は enforcement に影響させず、stderr に `maestro_policy_shadow_error ...` を出す

### Step 7: feature flag で薄い wrapper に切り替え ⚠ Experimental (2026-04-27)

- `templates/config.yaml` / `model.WorkerConfig` に `agents.workers.policy_hook_implementation` を追加
- valid values: `bash` (default), `shadow`, `go`
- `bash`: legacy bash hook を authoritative に実行
- `shadow`: legacy bash hook を authoritative に実行し、Go 判定との差分を stderr に記録
- `go`: `maestro hook policy-check` へ委譲する薄い wrapper を生成
- launcher は `.maestro/config.yaml` から flag を読み、`.maestro/hooks/worker-policy.sh` の生成内容を切り替える

**安全ゲート (2026-04-27 追補)**: `go` は parity 未達のため `internal/model/config_validate.go` でデフォルト reject される。parity-test ドライバや実験運用のみ環境変数 `MAESTRO_POLICY_HOOK_ALLOW_GO_UNSAFE=1` でバイパス可能。理由: 現時点で `internal/validate/policy.CheckWorkerPolicy` は ~10 ルールしか翻訳していないため、ゲートなしで `go` を選ぶと RUN_ON_MAIN / Write/Edit checks / `.maestro/` 書込み保護 / D002/D004(`git checkout -- .`)/D006/D009-D012/B001-B004/M-AGT1 等のほぼ全ルールが silently 無効化される。Step 8 達成 (corpus 拡張 + shadow divergence ゼロ) と同時にゲートを解除する。

### Step 8: bash ロジックの削除 (安全ゲートにより保留)

- 2026-04-27 時点では実施しない
- 理由: `internal/validate/policy.go` はまだ bash hook の全 rule を翻訳していない (例: RUN_ON_MAIN の Bash mutation denylist、Write/Edit path checks、`.maestro/` control-plane write checks、D002/WT001 など)
- 旧 bash logic を削除すると Worker safety coverage が低下するため、`bash` default と `shadow` 運用で divergence を潰してから削除する
- Step 8 の実施条件:
  - corpus が legacy bash の主要 117 hook tests 相当まで拡張済み
  - `shadow` mode の実運用で divergence ゼロ
  - `go` mode が default で安定運用済み

## リスクと対策

| リスク | 対策 |
|---|---|
| bash と Go の正規表現挙動差 (`(?i)` / `\b` / Unicode) | Step 4 の parity test で補足 |
| Worker 起動経路の追加遅延 (`exec maestro hook ...` で 10-50ms) | 実測 — Claude Code の tool call 全体 (秒オーダー) から見れば許容範囲。daemon UDS RPC への移行は overengineering |
| `maestro` バイナリ未配置時の fail-open | Go 関数は静的にコンパイルされた `maestro` 内、bash hook ラッパが exec failure 時 fail-closed (deny) する |
| 既存テスト 117 件の互換性 | Step 1 で string-level テストの一部は bash file 読み込みに変えたが、すべて green を確認済み。Step 2 で corpus 化した後、Go 関数のテストが SSOT になる |

## 「やらない」判断はなぜ採用しないか

F-026 と同じ「設計判断で実装回避」のパターンも検討したが、F-025 は本質的に異なる:

- F-026 は「tmux user-variable のままが安全」という設計判断 (代替案も同じ failure mode を持つ)
- F-025 は「Go 化は本物の改善だが工数とリスクが大きい」という工数判断

→ F-025 は「やる価値はある、ただし完了まで時間がかかる」が正しい。今 sprint で Step 1 (可読性向上)、Step 3 / 4 (Go 判定の一部翻訳 + parity test 足場)、Step 5 (CLI subcommand) までを着地させ、Step 2 / 6-8 は別 sprint で順次進める。

## 関連 Finding

- F-026 (Medium): `tmux user-variable` 経由の `@run_on_main` 配信 — 設計判断で実装回避済
- F-027 (Low): `--force-with-lease` の Worker 適用外明記 — 完了済
- F-053 (Medium): `policy_checker_test.go` dead code 削除 — 完了済
