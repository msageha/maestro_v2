# Skill Anatomy 規約

Maestro の worker/planner/orchestrator/share スキル (`templates/skills/<role>/<name>/SKILL.md`) が満たすべき構造規約。`internal/daemon/skill` の validator (`ValidateSkillTree`) が `templates/skills/` を single source of truth として機械検証し、CI の `go test ./...` で実行される (`anatomy_test.go`)。

`.maestro/skills/` は `maestro setup` の展開物なので検証対象ではない (repair は template artifact を上書きする)。規約を変えるときは `templates/skills/` を直し、setup/repair 経由で展開する。

## ディレクトリ構造

```
templates/skills/
  <role>/            # worker | planner | orchestrator | share
    <skill-name>/
      SKILL.md
```

`<skill-name>` は kebab-case の識別子 (英小文字・数字・ハイフン)。

## Frontmatter スキーマ

`SKILL.md` 冒頭の YAML frontmatter (`---` で囲む) は次を持つ。

| キー          | 必須 | 制約                                                           |
| ------------- | ---- | -------------------------------------------------------------- |
| `name`        | ✅   | **ディレクトリ名と完全一致**すること                           |
| `description` | ✅   | 1〜1024 バイト。何のためのスキルかを 1 文で                    |
| `version`     | ✅   | セマンティックバージョン文字列 (例 `"1.0.0"`)                  |
| `tags`        | ✅   | 文字列配列。最低 1 要素。role や領域を表す                     |
| `priority`    | ✅   | 整数。小さいほど優先。context budget 超過時に大きい値から drop |
| `mode`        | 任意 | 特殊注入モード (通常は省略)                                    |

`name == ディレクトリ名` は hard rule。フォールバック解決 (frontmatter name 一致) が破綻しないための不変条件。

## 本文の推奨セクション

見出し名は強制しない (既存スキルは日本語の番号セクションを使う)。ただし、エージェントの遵守率を上げる実証パターンとして次の 3 セクションを**推奨**する。validator は不在を warning として報告するが CI は落とさない。

1. **Common Rationalizations (よくある言い訳)** — エージェントが手順を飛ばすときの典型的な言い訳と、それがなぜ間違いかを対にして列挙する。「時間がないので既存テストの確認は省略する」→「確認漏れは後段の統合失敗として跳ね返り、結局遅くなる」のように。
2. **Red Flags (危険信号)** — 規律違反が起きているサイン。「テストを消してビルドを緑にしようとしている」「型エラーを `as any` で黙らせようとしている」など、自己点検のチェックポイント。
3. **Verification (完了チェックリスト)** — スキルが「守られた」と言える完了条件。観測コマンドとその出力を伴う証跡 (#14 と同じ精神) を求める。

これらは prose skill の遵守率を上げるための様式であり、見出しの言語・番号付けは自由。

## Cross-reference の整合

本文中で他スキルを参照するとき (例: `skill_refs: ["source-grounded-response"]` の記述、あるいは prose 中の参照) は、参照先が `templates/skills/` に実在するスキル名でなければならない。validator は本文の `skill_refs` 配列を抽出し、未実在の参照を error にする。

## Validator のルールと severity

| ルール                                       | severity | CI         |
| -------------------------------------------- | -------- | ---------- |
| `SKILL.md` が存在する                        | error    | 落とす     |
| frontmatter に `name` / `description` がある | error    | 落とす     |
| `name` == ディレクトリ名                     | error    | 落とす     |
| `description` が 1〜1024 バイト              | error    | 落とす     |
| `version` / `tags` / `priority` がある       | error    | 落とす     |
| `tags` が空でない                            | error    | 落とす     |
| 本文が空でない                               | error    | 落とす     |
| スキル名が一意 (role をまたいで重複しない)   | error    | 落とす     |
| `skill_refs` の参照先が実在する              | error    | 落とす     |
| 推奨 3 セクションが揃っている                | warning  | 落とさない |

## Exemption

除外はスキルの frontmatter ではなく **validator 側 (`internal/daemon/skill/anatomy.go`) に理由付きで持つ** (`recommendedSectionExemptSkills` マップ)。frontmatter に書かせると寄稿者が自己除外できてしまい規律が形骸化するため。除外は推奨セクション warning のみに適用でき、hard rule (error) からは除外できない。
