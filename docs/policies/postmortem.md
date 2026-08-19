# ポストモーテムポリシー

## 目的

このポリシーは、重大な事故・near miss・安全装置の頻繁な作動・想定外の手動介入から、Agentic Loop自身が学び、再発防止策を検証済みの変更としてmergeされるまで追跡することを目的とする。すべての機能・設計・運用に適用する。

## 非難しない原則

ポストモーテムは個人・利用者・特定のAIプロバイダの評価や非難に使わない。「注意不足だった」「気をつける」など個人依存の結論だけで完了させず、再現可能な仕組み（policy、Agent指示、テスト、観測性）の変更へ落とす。重大度や影響を根拠なく誇張しない。記録量そのものを成果指標にせず、再発防止の実装と検証を成果とする。事故対応中は復旧を優先し、分析はその後に行う。

## 起動基準

次のいずれかに該当する事象を、ポストモーテムの候補とする。

- 同種の失敗が一定回数反復した
- rate limit、quota、token、ディスクその他の有限資源を枯渇させた、または枯渇寸前になった
- worker、queue、deploy、releaseが長時間停止した
- 復旧や完遂に通常想定しない手動介入を要した
- confidentiality、integrity、availability、データ損失、追加費用、不変条件違反の実害またはnear missがあった
- 安全装置が作動したが、その手前の通常設計で回避できた
- 小規模では成功したが、わずかな規模増加で破綻することが判明した

利用者またはworkerは、上記に当たらない事象でも明示的にポストモーテムを要求できる（`bin/agentic-loop postmortem create`）。

### 自動起動（起動閾値は設定可能・境界付き）

自動起動は次の2経路に限定する（他の起動基準は明示要求で候補化する。詳細は[ADR 0026](../decisions/0026-postmortem-closed-loop.md)の残作業を参照）。

- **反復失敗**: Issueがリトライ予算を使い切って`agent:parked`へ移った時点（既存の`queue.max_attempts`、[ADR 0016](../decisions/0016-failure-park-not-close.md)をそのまま再利用し、専用の閾値を新設しない）。
- **有限資源の枯渇**: 全provider poolが利用不可になった（`agent-exhausted`/`all-pools-paused`）最初の遷移。

いずれも`.agentic-loop.toml`の`[postmortem]`で制御する。

- `auto_detect`（`on`/`off`、既定`on`）: 自動起動そのものの有効・無効。
- `max_auto_created_per_day`（1〜20の整数、既定5）: 1日（UTC）あたり自動作成できるポストモーテムIssueの総数の上限。この上限は**自動起動のみ**に適用する。上限に達した自動起動候補は作成せずログに残すだけで、supervisorのpollは止めない。利用者またはworkerによる明示要求（`bin/agentic-loop postmortem create`）は上限で拒否しない（ただし作成数は同じ日次counterへ加算するため、明示要求が多い日は後続の自動起動が先に抑制される）。

同一事象（種別+対象のfingerprint）に対応する**open**なポストモーテムIssueが既に存在する場合は、新規作成せず既存Issueへ追記する（重複抑止）。過去のポストモーテムが解決済み（closed）の状態で同じ事象が再発した場合は、再発として新しいIssueを作成する。

## 重大度

重大度は事実（実害の範囲、復旧に要した時間、再発回数）から導出し、推測や印象で誇張・過小評価しない。confidentiality/integrity/availabilityへの実害があった場合は、対応する`category:confidentiality-incident`/`category:integrity-incident`/`category:availability-incident`を付与する。実害がない反復失敗・資源枯渇・near missは`category:loop-continuity`を既定とする。

## 分析項目

ポストモーテムIssueは、再現可能な事実と推測を区別して次を記録する。

- 事象、時系列、影響範囲、検出方法、復旧内容
- 直接の引き金
- 構造的原因と寄与要因
- なぜ設計・plan・review・テスト・検証・監視で事前検出できなかったか
- 既存の要求・不変条件・policy・文書・テスト・観測性の不足またはdrift
- うまく働いた安全装置と、働かなかった防御層
- 個別修正と、他機能にも適用できる一般化された改善
- 残余リスク（対応しない項目がある場合はその理由）

秘密情報、脆弱性の悪用詳細、個人情報、不要なログ本文は記録・転記しない（`.agentic-loop/guard-secrets.sh`によるfail-closedな走査を経由する）。

## action item

各action itemは、担当範囲・優先度・依存関係・完了条件を持つ既存Issueまたは新規Issue（通常の`submit-requirement`のqueue-first intake）へ対応づける。個別の不具合修正と、横断的なpolicy・テスト・観測性・開発ループ改善は区別する。

ポストモーテムIssueは、`bin/agentic-loop postmortem link ISSUE ACTION_ISSUE...`でaction item Issueを native sub-issue・native dependency（`blocked_by`）として結び付け、`agent:blocked`へ移る。これは既存の依存関係機構（[Issueキュー運用](../operations/issue-queue.md)）をそのまま再利用し、専用の待機機構を新設しない。action itemがすべて完了・検証されると、既存の`requeue_dependency_ready`が自動的にポストモーテムIssueを`agent:queued`へ戻す。

## 完了条件

ポストモーテムIssueは、すべての必須action itemが検証済みになるまで未完了として追跡する（本文だけをcloseしてaction itemを未追跡にしない）。action itemのmerge後、再キューされたポストモーテムIssueは次を確認してから完了する。

- 原因と対策の対応が取れている
- 再現fixtureまたは同等の検証手段がある
- 回帰防止（テスト・lint・監視）が入っている
- 残余リスクが明記されている

実施しない項目がある場合は、理由と受容する残余リスクを明記したうえで完了させてよい。`bin/agentic-loop postmortem complete ISSUE`が、全action itemの検証済み・本文プレースホルダの記入済み・残余リスク節の非空を機械的に確認し、いずれか不成立ならIssueをcloseせず日本語で理由を返す（詳細は[運用文書](../operations/postmortem.md)）。

## 発見の一般化

一般化した知見は、`AGENTS.md`、`docs/policies/`、skill、worker prompt、共通検証入口（`devbox run --pure check`）のいずれかへ、文書だけでなく自動検証可能な形（lint、テスト）で反映する。手順は[postmortem skill](../../.agents/skills/postmortem/SKILL.md)を参照。

## 費用・外部サービス

外部の有料incident管理サービスを必須にしない。ポストモーテムの検出・作成・追跡自体が、大量のAPI呼び出し・無制限な履歴取得・無限retryを生まないよう、[費用ポリシー](cost.md)に従う（`max_auto_created_per_day`の上限、dedupによる重複抑止、既存の依存関係pollへの相乗り）。
