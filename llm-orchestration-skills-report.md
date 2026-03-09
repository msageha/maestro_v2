# LLM Orchestration スキル調査レポート

全9リポジトリのClaude Code / Copilot向けスキルを調査し、LLM Orchestrationシステムへの有用性を評価した統合レポート。

---

## 1. リポジトリ別スキル一覧表

### 1.1 openai/skills（37スキル）

| 有用性 | スキル名 | 概要 |
|--------|---------|------|
| 最高 | skill-creator | スキル設計パターン。メタスキルとしてスキル自動生成に応用可能 |
| 最高 | notion-spec-to-implementation | 仕様→タスク分解→進捗追跡の完全ワークフロー |
| 最高 | gh-address-comments | PRレビューコメントへの自動対応 |
| 最高 | gh-fix-ci | CI失敗の自動診断・修復 |
| 最高 | linear | タスク・スプリント管理の自動化 |
| 中 | security系（3スキル） | セキュリティスキャン・脆弱性分析 |
| 中 | playwright系（2スキル） | E2Eテスト自動化 |
| 中 | notion系（3スキル） | ドキュメント・ナレッジ管理 |
| 中 | sentry | エラートラッキング連携 |
| 低 | その他（22スキル） | 個別ツール連携・ユーティリティ |

### 1.2 testdino-hq/playwright-skill（5スキルパック、70+ガイド）

| 有用性 | スキル名 | 概要 |
|--------|---------|------|
| 中 | 全体（5パック） | QAエージェントのテスト知識ベース。Golden Rulesはテスト品質制約として参考 |

### 1.3 schalkneethling/webdev-agent-skills（7スキル）

| 有用性 | スキル名 | 概要 |
|--------|---------|------|
| 中 | component-usage-analysis | 依存関係分析。影響範囲調査に応用可能 |
| 中 | frontend-security | フロントエンドセキュリティチェック |
| 中 | frontend-testing | フロントエンドテスト戦略 |
| 低 | その他（4スキル） | クロスプラットフォーム配布パターン等 |

### 1.4 addyosmani/web-quality-skills（6スキル）

| 有用性 | スキル名 | 概要 |
|--------|---------|------|
| 中 | 全体（6スキル） | Web品質特化。QAタスクのチェックリスト・acceptance_criteria設計に参考 |

### 1.5 github/awesome-copilot（200+スキル）

| 有用性 | スキル名 | 概要 |
|--------|---------|------|
| 最高 | agent-governance | ガバナンス・信頼制御。Agentの行動制約設計 |
| 最高 | agentic-eval | 自己評価・改善ループ |
| 最高 | create-implementation-plan | AI-to-AI通信に最適化された計画生成 |
| 最高 | structured-autonomy系 | 計画→生成→実装の段階的自律パイプライン |
| 最高 | create-specification | AI消費に最適化された仕様書生成 |
| 最高 | breakdown-epic系 | 多角的タスク分解（複数戦略） |
| 中 | create-agentsmd | Agent設定ファイル自動生成 |
| 中 | finalize-agent-prompt | プロンプト最適化 |
| 中 | boost-prompt | プロンプト強化 |
| 中 | context-map | コンテキストマッピング |
| 中 | model-recommendation | モデル選定支援 |
| 中 | polyglot-test-agent | 多言語テストエージェント |
| 低 | その他（180+スキル） | 個別ツール・言語特化スキル |

### 1.6 anthropics/skills（17スキル）

| 有用性 | スキル名 | 概要 |
|--------|---------|------|
| 最高 | skill-creator | eval/iterate/benchmarkパターンによるスキル品質保証 |
| 最高 | mcp-builder | 4フェーズ開発プロセス（設計→実装→テスト→統合） |
| 最高 | claude-api | LLM基盤技術。API活用パターン |
| 中 | doc-coauthoring | 段階的ワークフロー・sub-agent活用パターン |
| 中 | webapp-testing | Webアプリテスト自動化 |
| 低 | その他（12スキル） | 個別ツール連携 |

### 1.7 vercel-labs/agent-skills（5スキル）

| 有用性 | スキル名 | 概要 |
|--------|---------|------|
| 中 | deploy-to-vercel | アクション系スキル設計パターンの参考 |
| 低 | その他（4スキル） | フロントエンド特化。Orchestrationへの直接有用性は限定的 |

### 1.8 phuryn/pm-skills（65スキル、8プラグイン）

| 有用性 | スキル名 | 概要 |
|--------|---------|------|
| 最高 | create-prd | プロダクト要件定義の構造化生成 |
| 最高 | brainstorm-okrs | OKR策定の体系的アプローチ |
| 最高 | outcome-roadmap | 成果ベースロードマップ生成 |
| 最高 | sprint-plan | スプリント計画の自動化 |
| 最高 | pre-mortem | リスク事前分析 |
| 最高 | prioritization-frameworks | 優先度判定フレームワーク |
| 最高 | identify-assumptions | 前提条件の特定・検証 |
| 最高 | prioritize-assumptions | 前提条件の優先度判定 |
| 最高 | prioritize-features | 機能優先度判定 |
| 最高 | opportunity-solution-tree | 機会→解決策のツリー構造化 |
| 中 | retro | 振り返り・改善提案 |
| 中 | stakeholder-map | ステークホルダー分析 |
| 中 | user-stories / job-stories | ユーザーストーリー生成 |
| 中 | test-scenarios | テストシナリオ設計 |
| 中 | north-star-metric | 北極星指標の設定 |
| 低 | その他（50スキル） | PM特化ツール |

### 1.9 googleworkspace/cli skills（37スキル）

| 有用性 | スキル名 | 概要 |
|--------|---------|------|
| 最高 | gws-modelarmor系 | AIセーフティ・入出力フィルタリング |
| 最高 | gws-workflow | マルチステップワークフロー設計 |
| 最高 | gws-workflow-email-to-task | 入力→タスク変換の自動化パターン |
| 最高 | persona-project-manager | PMペルソナ設計の参考実装 |
| 中 | ペルソナ系（10スキル） | persona_hint設計の参考 |
| 中 | gws-events系 | イベント駆動パターン |
| 中 | gws-shared | 共通基盤スキル設計パターン |
| 低 | その他（20+スキル） | Google Workspace個別操作 |

---

## 2. 推奨スキルサマリー（機能カテゴリ別）

### 2.1 タスク分解・計画

LLM Orchestrationの根幹機能。大きな目標を実行可能な単位に分割し、依存関係を管理する。

| 推奨スキル | リポジトリ | 活用ポイント |
|-----------|-----------|-------------|
| breakdown-epic系 | github/awesome-copilot | 多角的分解戦略（機能・技術・リスク軸）の実装パターン |
| create-implementation-plan | github/awesome-copilot | AI-to-AI通信に最適化された計画フォーマット |
| structured-autonomy系 | github/awesome-copilot | 計画→生成→実装の段階的パイプライン設計 |
| sprint-plan | phuryn/pm-skills | スプリント単位の作業計画自動化 |
| notion-spec-to-implementation | openai/skills | 仕様→タスク分解→進捗追跡の一貫ワークフロー |
| opportunity-solution-tree | phuryn/pm-skills | 問題空間から解決策への構造的マッピング |
| create-prd | phuryn/pm-skills | 要件定義の構造化テンプレート |

### 2.2 品質管理・検証

成果物の品質を保証するための検証・レビュー・テスト機能。

| 推奨スキル | リポジトリ | 活用ポイント |
|-----------|-----------|-------------|
| agentic-eval | github/awesome-copilot | Agent出力の自己評価・改善ループ実装 |
| gh-fix-ci | openai/skills | CI失敗時の自動診断・修復フロー |
| gh-address-comments | openai/skills | PRレビュー対応の自動化 |
| skill-creator（anthropic版） | anthropics/skills | eval/iterate/benchmarkによるスキル品質保証 |
| test-scenarios | phuryn/pm-skills | テストシナリオの体系的生成 |
| web-quality-skills全体 | addyosmani | acceptance_criteria設計のチェックリスト参考 |

### 2.3 ガバナンス・安全制御

Agent行動の制約・監視・安全性を確保する機能。

| 推奨スキル | リポジトリ | 活用ポイント |
|-----------|-----------|-------------|
| agent-governance | github/awesome-copilot | 信頼レベル制御・行動制約の設計パターン |
| gws-modelarmor系 | googleworkspace | AI入出力のセーフティフィルタリング |
| pre-mortem | phuryn/pm-skills | タスク実行前のリスク事前分析 |
| security系 | openai/skills | セキュリティスキャン・脆弱性チェック |

### 2.4 ワークフロー設計・自動化

複数ステップにまたがる処理の定義・実行・監視。

| 推奨スキル | リポジトリ | 活用ポイント |
|-----------|-----------|-------------|
| gws-workflow | googleworkspace | マルチステップワークフロー定義パターン |
| gws-workflow-email-to-task | googleworkspace | 外部入力→タスク変換の自動化 |
| mcp-builder | anthropics/skills | 4フェーズ（設計→実装→テスト→統合）開発プロセス |
| linear | openai/skills | タスク・スプリント管理との連携 |

### 2.5 ペルソナ・役割設計

Agentに特定の役割・視点を付与する機能。

| 推奨スキル | リポジトリ | 活用ポイント |
|-----------|-----------|-------------|
| persona-project-manager | googleworkspace | PMペルソナの具体的実装例 |
| ペルソナ系（10スキル） | googleworkspace | persona_hint設計の多様なパターン参考 |
| create-agentsmd | github/awesome-copilot | Agent設定ファイルの自動生成 |
| finalize-agent-prompt | github/awesome-copilot | ペルソナプロンプトの最適化手法 |

### 2.6 メタスキル・スキル管理

スキル自体の設計・生成・評価を行う機能。

| 推奨スキル | リポジトリ | 活用ポイント |
|-----------|-----------|-------------|
| skill-creator（openai版） | openai/skills | スキル設計テンプレート・パターン |
| skill-creator（anthropic版） | anthropics/skills | eval/iterate/benchmarkベースのスキル品質保証 |
| boost-prompt | github/awesome-copilot | プロンプト品質の自動強化 |
| create-specification | github/awesome-copilot | AI消費に最適化された仕様書フォーマット |

### 2.7 優先度判定・意思決定

タスクや機能の優先度を体系的に判定する機能。

| 推奨スキル | リポジトリ | 活用ポイント |
|-----------|-----------|-------------|
| prioritization-frameworks | phuryn/pm-skills | RICE/ICE等の優先度判定フレームワーク |
| prioritize-features | phuryn/pm-skills | 機能レベルの優先度自動判定 |
| prioritize-assumptions | phuryn/pm-skills | 前提条件の重要度・リスク評価 |
| brainstorm-okrs | phuryn/pm-skills | 目標設定と成果指標の構造化 |

---

## 3. Maestroシステムへの適用提案

### 3.1 Orchestrator への適用

Orchestratorはコマンドの受付・全体進捗監視・最終判断を担う。

| 適用スキルパターン | 参考元 | 適用方法 |
|-------------------|--------|---------|
| **agent-governance** | github/awesome-copilot | Orchestratorの安全制御ポリシーの強化。Agent間の信頼レベル管理、破壊的操作の追加ガードレール設計 |
| **gws-modelarmor系** | googleworkspace | Orchestratorが受け取るユーザー入力のサニタイズ・安全性検証。プロンプトインジェクション防御の強化 |
| **agentic-eval** | github/awesome-copilot | コマンド完了時の最終品質評価。全Worker結果の統合評価ロジック |
| **north-star-metric** | phuryn/pm-skills | コマンド成功の定量的評価基準の設計 |

### 3.2 Planner への適用

Plannerはタスク設計・分解・依存関係定義・フェーズ構成を担う。

| 適用スキルパターン | 参考元 | 適用方法 |
|-------------------|--------|---------|
| **breakdown-epic系** | github/awesome-copilot | タスク分解アルゴリズムの改善。機能軸・技術軸・リスク軸の多角的分解戦略の導入 |
| **create-implementation-plan** | github/awesome-copilot | Plannerが生成するタスクのフォーマット最適化。Worker（LLM Agent）が消費しやすい構造化タスク記述 |
| **structured-autonomy系** | github/awesome-copilot | フェーズ設計の改善。計画→調査→実装→検証の段階的パイプライン設計 |
| **sprint-plan** | phuryn/pm-skills | フェーズ内タスクの並列度・依存関係の最適化 |
| **create-prd / create-specification** | phuryn/pm-skills, awesome-copilot | タスクの `content` / `acceptance_criteria` の品質向上テンプレート |
| **pre-mortem** | phuryn/pm-skills | タスク設計時のリスク事前分析。失敗パターンの予測と`constraints`への反映 |
| **prioritization-frameworks** | phuryn/pm-skills | タスク優先度の体系的判定。クリティカルパスの特定 |
| **opportunity-solution-tree** | phuryn/pm-skills | 要件から実装タスクへの構造的マッピング |
| **notion-spec-to-implementation** | openai/skills | 仕様書からタスク分解への変換パイプライン |
| **test-scenarios** | phuryn/pm-skills | verificationフェーズのタスク設計。acceptance_criteriaの体系的生成 |

### 3.3 Worker への適用

Workerはタスク実行・品質検証・結果報告を担う。

| 適用スキルパターン | 参考元 | 適用方法 |
|-------------------|--------|---------|
| **gh-fix-ci** | openai/skills | Workerのverificationタスクにおけるビルド・テスト失敗時の自動修復パターン |
| **gh-address-comments** | openai/skills | レビュー指摘への自動対応。Workerが生成したコードへのフィードバックループ |
| **mcp-builder** | anthropics/skills | 4フェーズ開発（設計→実装→テスト→統合）のWorkerタスク実行フレームワーク |
| **skill-creator（両版）** | openai, anthropics | Workerが新スキルを自動生成・登録する機能。eval/benchmarkによる品質保証 |
| **security系** | openai/skills | Workerの実装タスクにおけるセキュリティチェック自動実行 |
| **doc-coauthoring** | anthropics/skills | ドキュメント生成タスクにおける段階的ワークフロー・sub-agent活用パターン |
| **component-usage-analysis** | schalkneethling | 影響範囲調査。`Explore` SubAgentへの委譲パターンの改善 |

### 3.4 ペルソナ（persona_hint）設計への適用

| 適用スキルパターン | 参考元 | 適用方法 |
|-------------------|--------|---------|
| **persona-project-manager** | googleworkspace | `architect` ペルソナの改善参考。PM視点での設計判断パターン |
| **ペルソナ系（10スキル）** | googleworkspace | 新規ペルソナの追加検討。specialist, reviewer, researcher等の行動指針設計 |
| **finalize-agent-prompt** | github/awesome-copilot | ペルソナプロンプトの品質最適化。曖昧さの排除、指示の明確化 |
| **polyglot-test-agent** | github/awesome-copilot | `quality-assurance` ペルソナの多言語対応パターン |

### 3.5 システム全体への横断的適用

| 適用スキルパターン | 参考元 | 適用方法 |
|-------------------|--------|---------|
| **gws-workflow** | googleworkspace | Maestroのコマンドパイプライン（command→phase→task）の設計改善。ワークフロー定義DSLの参考 |
| **gws-workflow-email-to-task** | googleworkspace | 外部入力（Issue, PR, Slack等）からMaestroコマンドへの自動変換 |
| **gws-shared** | googleworkspace | 共通基盤スキル設計パターン。Maestroの共通プロンプト（maestro.md）の改善 |
| **context-map** | github/awesome-copilot | コンテキストウィンドウの効率的利用。タスク間の情報伝達最適化 |
| **create-agentsmd** | github/awesome-copilot | Maestroの設定ファイル（config.yaml, instructions/）の自動生成・更新 |
| **boost-prompt** | github/awesome-copilot | 全Agent（Orchestrator/Planner/Worker）のプロンプト品質自動改善 |

---

## 4. 総括

### 特に注目すべきリポジトリ（Top 3）

1. **github/awesome-copilot** — 200+スキルの中にLLM Orchestrationの核心機能（ガバナンス・自己評価・構造化自律・タスク分解）に直接活用できるパターンが豊富
2. **phuryn/pm-skills** — PM手法をスキル化しており、Plannerのタスク分解・優先度判定・リスク分析の改善に直結
3. **anthropics/skills + openai/skills** — スキル設計のメタパターン（skill-creator）と実用的CI/PR自動化スキルの組み合わせ

### 推奨アクション

1. **短期**: gh-fix-ci、gh-address-commentsをWorkerスキルとして導入し、CI/PRワークフローを自動化
2. **中期**: breakdown-epic系、create-implementation-planのパターンをPlannerのタスク分解ロジックに反映
3. **長期**: agent-governance、agentic-evalのパターンでOrchestratorのガバナンス・品質評価機能を強化
