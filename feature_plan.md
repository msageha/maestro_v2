# Feature Plan — Maestro v2 新機能実装方針

## 参考リポジトリから得た知見

### multi-agent-shogun
- bash 100% 実装の tmux ベースマルチエージェントシステム。戦国時代メタファー（将軍→家老→足軽/軍師）
- **活用可能なパターン**:
  - 4層ロール階層: Gunshi（軍師）= 非実装の戦略分析専門ロール。maestro_v2 の Analyst/Reviewer ロール検討材料
  - マルチプロバイダー CLI アダプター: `lib/cli_adapter.sh` が Claude/Codex/Copilot/Kimi の差異を吸収。`instructions/cli_specific/` で CLI 固有指示を分離
  - 4層コンテキストモデル: Layer1(Memory MCP 永続) / Layer2(プロジェクトファイル) / Layer3(YAML Queue) / Layer4(セッション)
  - Skill Candidate 自動収集: Worker の report に skill_candidate フィールド → Karo 集約 → Shogun 承認
  - Event-Driven Wait: dispatch 後に STOP、inbox wakeup で再開。'Wake = Full Scan' パターン

### maestro v1
- シェルスクリプト実装の前身。3層階層 + fswatch イベント駆動
- **v1 にあって v2 にない機能（今回の5機能に関連するもの）**:
  - マルチプロバイダー対応（Claude/Codex/Gemini の3プロバイダー）
  - スキル管理（skill_candidate の体系的収集・承認フロー）
  - memory.jsonl（永続学習記録、append-only）
  - Bloom's Taxonomy によるモデル割当（Sonnet/Opus 自動振り分け）
  - Continuous Mode（タスク→コミット→評価→自動継続）

### SuperClaude Framework
- Claude Code を構造化プラットフォームに変換するメタプログラミングフレームワーク（v4.2.0）
- **活用可能なパターン**:
  - ペルソナ設計: `src/superclaude/agents/` 内の Markdown ファイル。Frontmatter(name, description, category) + Triggers → Behavioral Mindset → Focus Areas → Key Actions → Outputs → Boundaries
  - スキル定義: `skills/` に YAML frontmatter + Markdown。重み付きスコアリング方式
  - ReflexionPattern: エラー学習のクロスセッションパターンマッチング
  - 構造化 Markdown テンプレート: ペルソナ・モード・スキル全てを Markdown + YAML frontmatter で統一

---

## 機能1: merge 後の worktree 削除

### 現状分析
- **ライフサイクル**: `CreateForCommand` → `EnsureWorkerWorktree` → `CommitWorkerChanges` → `MergeToIntegration` → `PublishToBase` → `CleanupCommand`
- **GC**: `GC()` メソッドで TTL 期限切れ + max_worktrees 超過の2条件でクリーンアップ。orphan worktree 検出あり（`manager.go:820-900`）
- **config**: `WorktreeConfig` に `cleanup_on_success` / `cleanup_on_failure` フィールドが存在するが、**未配線**（使用箇所なし）
- **CleanupCommand**: worktree 削除 → ブランチ削除 → ディレクトリ削除 → state ファイル削除（`manager.go:761-818`）
- **問題**: `CleanupCommand` はエラー発生時でも state ファイルを削除する（リトライ不可能）

### 実装方針

#### 変更ファイル
1. **`internal/daemon/queue_scan_phase.go`** — publish 完了後に `cleanup_on_success` をチェックし、`CleanupCommand` を呼び出すロジック追加
   - publish 後の同一スキャン内で cleanup をスケジュール（次スキャンでも recover 可能）
   - `cleanup_on_failure` も同様に command 失敗時の cleanup をトリガー

2. **`internal/daemon/worktree/manager.go`** — `CleanupCommand` の state ファイル削除タイミング修正
   - state ファイル削除は全 worktree の cleanup 成功後のみ実行
   - 部分失敗時は `cleanup_failed` ステータスを保持し、次スキャンでリトライ可能にする

3. **`internal/model/status.go`** — `IntegrationStatusCleanupFailed` ステータス追加（必要に応じて）

#### 設計判断
- **reconcile R7 パターンは不採用**: reconciler はクラッシュ/不整合修復専用。正常フローの cleanup は daemon post-publish フローで処理
- **即時削除 vs 遅延**: `cleanup_on_success=true` → publish 直後に cleanup。`false` → GC/TTL に委任（ユーザーが inspect 可能）
- GC は既存の TTL/max_worktrees ベースのフォールバックとして維持

### codex フィードバック
- `cleanup_on_success` を daemon の post-publish フローに配線するアプローチを支持
- R7 reconcile パターンは不適切（reconciler は crash/inconsistency repair 用）
- **重要な修正**: `CleanupCommand` が error 発生時でも state ファイルを削除する問題を指摘。state ファイルは全 cleanup 成功後にのみ削除し、部分失敗時は `cleanup_failed` を保持してリトライ可能にすべき
- daemon は `queue/` と `results/` のみ watch しているため、worktree state のイベント駆動は実質的に scan/reconcile pass になる

---

## 機能2: worker ペルソナ設定

### 現状分析
- **Agent 起動**: `launcher.go` で `buildSystemPrompt(maestroDir, role)` が `maestro.md` + `instructions/{role}.md` を連結
- **config**: `WorkerConfig{Count, DefaultModel, Models map[string]string, Boost}` — ペルソナフィールドなし
- **タスク配信**: `purpose`, `content`, `acceptance_criteria`, `constraints`, `tools_hint` フィールド
- **Worker 選択**: `worker_assign.go` で `model + pending count` のみ考慮
- **制約**: Worker は launch 時に system prompt が固定される。タスク配信で prompt は変更不可

### 実装方針

#### 変更ファイル
1. **`internal/model/config.go`** — `WorkerConfig` にペルソナ関連フィールド追加
   ```go
   type WorkerConfig struct {
       Count          int               `yaml:"count"`
       DefaultModel   string            `yaml:"default_model"`
       Models         map[string]string `yaml:"models,omitempty"`
       Boost          bool              `yaml:"boost"`
       DefaultPersona string            `yaml:"default_persona,omitempty"`
       Personas       map[string]string `yaml:"personas,omitempty"` // worker_id -> persona_name
   }
   ```

2. **`internal/model/queue.go`** — `QueueEntry`（タスク）に `persona_hint` フィールド追加

3. **`internal/agent/launcher.go`** — `buildSystemPrompt()` 拡張
   - ペルソナファイル（`.maestro/personas/{name}.md`）を読み込み
   - 注入順序: `maestro.md` → `personas/{name}.md` → `instructions/worker.md`（Worker ルールが最優先）

4. **`templates/personas/`** — デフォルトペルソナテンプレート追加（`generalist.md`, `backend-architect.md` 等）
   - `templates/embed.go` の `go:embed` 更新

5. **`internal/setup/init.go`** — `maestro init` 時にペルソナテンプレートを `.maestro/personas/` にコピー

6. **`internal/plan/worker_assign.go`** — Worker 選択ロジックに `persona_hint` マッチング追加
   - `persona_hint` が指定されている場合、一致するペルソナの Worker を優先的に割り当て

7. **`internal/formation/tmux_formation.go`** — tmux user variable に `@persona` を追加

#### 設計判断
- **ハイブリッド方式（Option C）採用**: config でデフォルトペルソナ、タスクで `persona_hint` によるオーバーライド
- **ペルソナストレージ**: `templates/personas/` に埋め込み（初期シード用）、`.maestro/personas/` から runtime 読み込み
- **真のタスク別ペルソナ切り替え**: Worker は launch 時に prompt 固定のため、`persona_hint` は (1) Worker 選択の優先度調整 + (2) タスク content への行動指針注入 の2重で機能
- ペルソナ名は `validate.ValidateID()` で検証（パストラバーサル防止）

### codex フィードバック
- ハイブリッド方式を支持。ただし `persona_hint` がタスクごとに system prompt を書き換えるのではなく、Worker 選択 + タスク content 注入として機能すべき
- per-worker ペルソナ map は `Models map[string]string` と同じパターンで適切
- Worker assignment の拡張が重要: 現在の `model + pending count` に `persona` マッチングを追加しないと `persona_hint` は装飾的
- リトライ時にペルソナメタデータを保持する必要あり
- ペルソナファイルは runtime に `.maestro/personas/` から読み込み、`go:embed` は init シード用のみ

---

## 機能3: codex/gemini 対応（モデル名ベースの判別）

### 現状分析
- **Claude 専用ハードコード 10 箇所**:
  1. `launcher.go:96` — `exec.Command("claude", args...)`
  2. `launcher.go:119-157` — `buildLaunchArgs()` with Claude-specific flags
  3. `launcher.go:94-97` — CLAUDECODE 環境変数フィルタリング
  4. `executor.go:713` — `isPromptReady()` で `❯` / `>` 検出
  5. `executor.go:804` — `maestro agent launch` 再起動コマンド
  6. `formation/tmux_formation.go:133` — launch コマンド送信
  7. `formation/tmux_formation.go:198-221` — `resolveModel()` with "opus"/"sonnet"
  8. `worker/standby.go:158-171` — `resolveWorkerModel()` (重複)
  9. `templates/config.yaml:13-19` — デフォルトモデル名
  10. `busy_detector.go` — Claude Code UI 文字列パターン
- **resolveModel() 重複**: `formation/` と `worker/` に同一ロジック

### 実装方針

#### 新規パッケージ
**`internal/provider/`** — CLI プロバイダー抽象化レイヤー

```go
// provider.go
type Provider string
const (
    ProviderClaude Provider = "claude"
    ProviderCodex  Provider = "codex"
    ProviderGemini Provider = "gemini"
)

type LaunchSpec struct {
    Binary  string
    Args    []string
    Env     []string   // 除外すべき環境変数
}

type RuntimeSpec struct {
    PromptReady func(line string) bool
    BusyRegex   *regexp.Regexp
}

type Capabilities struct {
    SupportsAllowedTools    bool
    SupportsDisallowedTools bool
    SupportsSystemPrompt    bool
    SupportsSettings        bool
}

type Adapter interface {
    PrepareLaunch(role, model, systemPrompt string) LaunchSpec
    RuntimeSpec() RuntimeSpec
    Capabilities() Capabilities
}

// ResolveProvider determines provider from explicit config or model name
func ResolveProvider(explicit string, model string) Provider
// ResolveAgentRuntime resolves provider + model from config
func ResolveAgentRuntime(cfg model.AgentsConfig, agentID string) (Provider, string)
```

#### 変更ファイル
1. **`internal/provider/`** — 新規パッケージ
   - `provider.go` — インターフェース定義 + `ResolveProvider()`, `ResolveAgentRuntime()`
   - `claude.go` — Claude Code アダプター（現在の hardcode を移植）
   - `codex.go` — OpenAI Codex CLI アダプター
   - `gemini.go` — Gemini CLI アダプター

2. **`internal/model/config.go`** — `AgentConfig` に `Provider` フィールド追加
   ```go
   type AgentConfig struct {
       ID       string `yaml:"id"`
       Model    string `yaml:"model"`
       Provider string `yaml:"provider,omitempty"` // "claude", "codex", "gemini"
   }
   ```

3. **`internal/agent/launcher.go`** — provider adapter 経由で launch
   - `buildLaunchArgs()` を `adapter.PrepareLaunch()` に置換
   - 環境変数フィルタリングを `LaunchSpec.Env` から取得

4. **`internal/agent/executor.go`** — provider adapter 経由で prompt/busy 検知
   - `isPromptReady()` を `adapter.RuntimeSpec().PromptReady()` に置換
   - busy detection を provider-specific に変更（per-pane provider 解決）

5. **`internal/formation/tmux_formation.go`** — `resolveModel()` を `provider.ResolveAgentRuntime()` に統合
   - tmux user variable に `@provider` 追加

6. **`internal/worker/standby.go`** — `resolveWorkerModel()` 削除、共通 resolver を使用

7. **`internal/agent/busy_detector.go`** — provider-specific busy パターン

#### 設計判断
- **解決優先順位**: `explicit provider` > `model名からの推論` > `legacy default (opus/sonnet → claude)`
- **単一 Adapter インターフェース**: launch と runtime を分離せず、1つの adapter で一貫性を保つ。ただし `PrepareLaunch()` → `LaunchSpec` / `RuntimeSpec()` で出力は分離
- **Capabilities**: Claude の `--allowedTools`/`--disallowedTools` は provider 固有。他 provider が同等制限をサポートしない場合、非 worker ロールでは fail-closed
- **resolveModel() 統合**: `ResolveAgentRuntime()` で formation/ と worker/ の重複を解消
- **tmux state**: `@provider` を `@model` と並んで永続化（再起動パスで必要）

### codex フィードバック
- 明示的 `provider` フィールドを推奨（model名推論はフォールバックのみ）
- 単一 adapter インターフェースだが、output を `LaunchSpec` / `RuntimeSpec` に分離する設計を支持
- `Capabilities()` が重要: `--allowedTools`/`--disallowedTools` が使えない provider では非 worker ロールを fail-closed にすべき
- `resolveModel()` の統合を推奨: `ResolveAgentRuntime(cfg, agentID) -> {Role, Provider, Model}` を共通 resolver として作成
- `worker_assign.go` の Bloom level → model mapping も抽象化が必要（`standard`/`advanced` tier）
- executor の busy regex はグローバルではなく per-pane/provider で解決すべき

---

## 機能4: skills 選定

### 現状分析
- **テンプレート構造**: `templates/` に `config.yaml`, `maestro.md`, `instructions/{role}.md`, `dashboard.md` を `go:embed` で埋め込み
- **プロンプト構築**: `launcher.go:159-182` の `buildSystemPrompt()` で `maestro.md` + `instructions/{role}.md` を連結
- **タスクフィールド**: `tools_hint` がツール推奨として存在
- **learnings 注入**: `FormatLearningsSection()` でタスク content に付加

### 実装方針

#### 新規ファイル・ディレクトリ
1. **`templates/skills/`** — デフォルトスキルテンプレート
   - `confidence-check.md` — タスク着手前の自信度チェック
   - `code-review.md` — コードレビュー手順
   - `test-strategy.md` — テスト戦略策定
   - `security-audit.md` — セキュリティ監査手順
   - 各ファイルは YAML frontmatter (`name`, `description`, `triggers`) + Markdown 本文

2. **`templates/embed.go`** — `go:embed` 更新
   ```go
   //go:embed config.yaml dashboard.md maestro.md instructions skills
   var FS embed.FS
   ```

#### 変更ファイル
3. **`internal/model/queue.go`** — タスクに `skills` フィールド追加（`skills_hint` ではなく `skills`）
   ```go
   type QueueEntry struct {
       // ... existing fields ...
       SkillRefs []string `yaml:"skills,omitempty"` // スキル名のリスト
   }
   ```

4. **`internal/daemon/dispatcher.go`** — タスク配信時にスキル content をロード・注入
   - `.maestro/skills/{name}.md` からファイル読み込み
   - YAML frontmatter をストリップし、Markdown 本文のみをタスク content に付加
   - 注入優先順位: `acceptance_criteria` > `content` > `skills` > `learnings`

5. **`internal/setup/init.go`** — `maestro init` 時に `templates/skills/` → `.maestro/skills/` へコピー

6. **`internal/plan/input.go`** — Planner からのタスク入力に `skills` フィールド追加

7. **`internal/plan/submit.go`** — タスク submit 時に `skills` フィールドを queue entry に転写

#### スキルファイルフォーマット
```markdown
---
name: confidence-check
description: タスク着手前の自信度チェック
triggers:
  - complex implementation
  - architecture change
---

## Confidence Check

タスク実行前に以下を確認:
1. コードベースの関連箇所を十分に理解しているか
2. acceptance_criteria を満たす方法が明確か
3. 既存コードとの互換性に問題がないか

スコアが低い場合は、追加調査を行うか failed で報告する。
```

#### 設計判断
- **dispatch 時注入**: launch 時ではなく dispatch 時にスキル content を注入。理由: スキルはタスク固有、launch 後の変更が反映される、全タスクの prompt を肥大化させない
- **フィールド名**: `skills`（`skills_hint` ではない）。Planner が選定し system が適用する強い契約
- **auto-collection は v1 では不実装**: prompt supply chain リスク。`--learnings` を観察ストリームとして活用し、手動キュレーション後にスキル化
- **ストレージ**: `templates/skills/` は init シード、runtime は `.maestro/skills/` から読み込み
- **frontmatter**: name, description, triggers のみ。unknown keys は無視（将来のスコアリング追加に対応）

### codex フィードバック
- dispatch 時注入を強く推奨（launch 時は不適切: エージェント再起動が必要、全タスクに影響）
- Markdown + YAML frontmatter フォーマットを支持
- フィールド名は `skills` / `skill_refs` を推奨（`skills_hint` は advisory すぎる）
- auto-collection は v1 では不要。promotion にはセキュリティリスク（worker 生成テキストが将来の prompt に）
- 注入優先順位: `acceptance_criteria` > `content` > `skills` > `learnings` を提案
- runtime は `.maestro/skills/` から読み込み、embedded は backward-compatibility fallback のみ

---

## 機能5: メモリー機能

### 現状分析
- **learnings 実装**: `internal/daemon/learnings/learnings.go` — `ReadTopKLearnings()` + `FormatLearningsSection()`
- **永続化**: `.maestro/state/learnings.yaml` (`LearningsFile` 構造体)
- **config**: `LearningsConfig` — `enabled`, `max_entries`(100), `max_content_length`(500), `inject_count`(5), `ttl_hours`(72)
- **注入**: タスク content に `"\n\n---\n参考: 過去の学習知見\n"` として付加
- **収集**: Worker が `--learnings` フラグで報告 → `result_write_handler.go` で永続化
- **特性**: ephemeral（TTL ベース消滅、デフォルト72時間）、latest-K 選択、command スコープ

### 実装方針

#### 新規ファイル
1. **`internal/daemon/memory/memory.go`** — メモリ管理パッケージ
   ```go
   type MemoryEntry struct {
       ID        string   `yaml:"id"`
       Content   string   `yaml:"content"`
       Tags      []string `yaml:"tags,omitempty"`
       PathGlobs []string `yaml:"path_globs,omitempty"` // 関連ファイルパターン
       Source    string   `yaml:"source"`               // "manual", "promoted", "worker:{id}"
       Pinned    bool     `yaml:"pinned"`
       CreatedAt string   `yaml:"created_at"`
       UpdatedAt string   `yaml:"updated_at"`
   }

   type MemoryFile struct {
       SchemaVersion int           `yaml:"schema_version"`
       FileType      string        `yaml:"file_type"`
       Entries       []MemoryEntry `yaml:"entries"`
   }

   func ReadRelevantMemories(maestroDir string, cfg MemoryConfig, taskContent string) ([]MemoryEntry, error)
   func FormatMemorySection(entries []MemoryEntry) string
   ```

2. **`cmd/maestro/cmd_memory.go`** — CLI サブコマンド
   - `maestro memory add --content "..." [--tags tag1,tag2] [--pinned]`
   - `maestro memory list [--tag tag]`
   - `maestro memory remove <id>`
   - `maestro memory promote <learning-id>` — learning → memory 昇格

#### 変更ファイル
3. **`internal/model/config.go`** — `MemoryConfig` 追加
   ```go
   type MemoryConfig struct {
       Enabled          bool `yaml:"enabled"`
       MaxEntries       int  `yaml:"max_entries"`        // default: 50
       MaxContentLength int  `yaml:"max_content_length"` // default: 1000
       InjectCount      int  `yaml:"inject_count"`       // default: 3
   }
   ```
   - `Config` struct に `Memory MemoryConfig` フィールド追加

4. **`internal/daemon/dispatcher.go`** — タスク配信時にメモリ注入
   - 注入順序: メモリ（`参考: プロジェクトメモリ`）→ learnings（`参考: 過去の学習知見`）
   - メモリの relevance ranking: `pinned` 優先 → `path_globs` マッチ → `tags` マッチ → recency

5. **`internal/daemon/daemon_startup.go`** — memory 関連の初期化

6. **`internal/lock/lock_order_enabled.go`** — `state:memory` ロックキー追加（必要に応じて）

#### ストレージ
- **`.maestro/state/memory.yaml`** — YAML 形式（プロジェクト全体で統一、`AtomicWrite` 活用、手動編集可能）
- JSONL は不採用（update-in-place メタデータが必要、manual editing の価値）
- `up --reset` で memory は**クリアしない**（永続知識は保持すべき）

#### 設計判断
- **learnings とは別システム**: 意味論が異なる（learnings=短命・Worker生成・ノイジー、memory=永続・キュレーション・プロジェクトレベル）
- **明示的追加**: `maestro memory add` / `maestro memory promote`。auto-promotion は v2 以降（"有用だった" シグナルが現在存在しない）
- **relevance ranking**: latest-K ではなく relevance ベース。pinned → path/tag マッチ → recency
- **access_count は不保持**: dispatch ごとの write は hot path を write-heavy にする
- **ロック**: `state:memory` 専用ロック。`state:{command}` と同時保持しない（result_write からの promotion 時）
- **inject_count**: デフォルト 3（learnings の 5 より少なく、質重視）

### codex フィードバック
- learnings とは別システムにすることを強く推奨（Option B）。`persistent: true` フラグは lifecycle/config/injection/pruning ルールを混乱させる
- YAML を推奨（update-in-place メタデータ対応、手動編集可能、既存インフラ活用）
- relevance + caps 方式を推奨。latest-K は不適切
- access_count の dispatch ごと更新は禁止（lock-free read path を壊す）
- 全 write は daemon 所有（single-writer パターン維持）
- `up --reset` と memory の関係を明確にドキュメント化すべき（`state/` が transient という既存セマンティクスとの不整合に注意）
- v1 最小形: `MemoryConfig` + `MemoryEntry` + CLI + inject。auto-promotion は後回し

---

## 実装優先順位の推奨

| 優先度 | 機能 | 理由 | 複雑度 | 既存インフラ依存 |
|--------|------|------|--------|-----------------|
| 1 | merge 後の worktree 削除 | 既存 config フィールド（cleanup_on_success/failure）の配線のみ。変更箇所が少なく、既存バグ（state ファイル早期削除）の修正も含む | 低 | queue_scan_phase.go, manager.go |
| 2 | worker ペルソナ設定 | config + template + launcher の拡張。Worker 選択ロジックの改善が主な作業 | 中 | config.go, launcher.go, worker_assign.go |
| 3 | skills 選定 | ペルソナと template/inject メカニズムを共有。ペルソナ実装後に着手すると効率的 | 中 | dispatcher.go, templates/, queue.go |
| 4 | メモリー機能 | 新規パッケージ + CLI サブコマンド + relevance ranking。learnings インフラを参考にできるが独立した設計が必要 | 中〜高 | learnings.go, dispatcher.go, config.go |
| 5 | codex/gemini 対応 | 影響範囲が最も広い（10箇所のハードコード変更）。provider 抽象化レイヤーの新規設計が必要。他機能との依存関係なし | 高 | launcher.go, executor.go, formation/, worker/, busy_detector.go |

**推奨アプローチ**:
1. 機能1（worktree 削除）を先行実装 → 既存バグ修正 + 簡単な配線
2. 機能2（ペルソナ）+ 機能3（skills）を同時進行 → template/inject メカニズムの共通基盤を構築
3. 機能4（メモリ）→ learnings パターンを参考に独立実装
4. 機能5（マルチプロバイダー）→ 最後に実施。他機能が安定してから抽象化レイヤーを導入
