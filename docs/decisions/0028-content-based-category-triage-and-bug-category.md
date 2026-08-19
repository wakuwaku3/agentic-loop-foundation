# 0028: queued Issueのカテゴリを内容ベースで自動トリアージし、`category:bug`を追加する

- 状態: 採用
- 日付: 2026-08-19

## 背景

`reconcile_queued_categories`はカテゴリ未設定の`agent:queued` Issueへ、内容を読まず一律`category:improvement`を付与していた（Issue #167）。実測では`agent:queued`の大半が`category:improvement`となり、ループ継続性・不具合修正など重要な種別が埋もれていた。

`CATEGORY_LABELS`は`loop-continuity`/`confidentiality-incident`/`integrity-incident`/`availability-incident`/`feature`/`improvement`の6種のみで、既存動作の不具合修正を表すカテゴリが存在しなかった。実害を伴わない不具合（バグ）を、実害の確認が必要な incident と同列に扱うことも、単なる`improvement`に埋もれさせることも、どちらも不正確である。

## 判断

### `category:bug`を追加する

incident（confidentiality/integrity/availability、実害で分類）とは明確に区別し、既存動作が壊れている場合の修正を表す。queue rankは`availability-incident`の次、`feature`の前に置く（loop-continuity=0 < confidentiality-incident=1 < integrity-incident=2 < availability-incident=3 < bug=4 < feature=5 < improvement=6）。`CATEGORY_LABELS`・`setup_label`・`project_option_for_category`・`queue_rank_jq`・`refresh_supervisor_snapshot`の rank 式・ProjectのCategory single-select optionへ反映する。Projectは`setup_project_migrate_single_select_options`（`setup_project_migrate_status_options`と共通化）で既存Projectへ`Bug` optionを後付けする。

### カテゴリ未設定Issueをタイトル・本文・commentのキーワードで内容分類する

`reconcile_queued_categories`は、カテゴリが複数のIssueは従来どおり最上位のみを残す一方、カテゴリが**ゼロ**のIssueに限り、タイトル・本文・commentを`triage_category_from_text`でキーワード判定する。

- `loop-continuity`: supervisor/worker/queue/lease/claim/資源枯渇/有限資源/スケーラビリティ/モジュール分割/競合判定/ポストモーテム/postmortem/トレーサビリティ/traceability/ワークツリー/worktree など、ループ自身の運用語。
- `bug`: バグ/不具合/誤動作/regression/crash/直らない/動作しない/壊れる など、既存動作の破綻を表す語。
- `feature`: 新機能/を実装する など、新規capabilityを表す語。
- いずれにも該当しなければ`category:improvement`（従来どおりの安全な既定値）。

判定はこの優先順位（rankと同じ順）で最初に一致した種別を採用する。人手で付与済みの単一カテゴリは対象外（従来どおり上書きしない）。

### incidentカテゴリは自動分類の対象外とする

`confidentiality-incident`/`integrity-incident`/`availability-incident`は、自由記述のキーワード一致では**絶対に**推測しない。incidentの分類は実害（confidentiality/integrity/availabilityへの実際の損害）の確認を要し（[0026](0026-postmortem-closed-loop.md)、docs/policies/postmortem.md）、自動化されたキーワード照合ではその実害を証明できない。誤ってincidentと自動判定すると、実害のない要求が誤って重大インシデント扱いされる/実際のincidentが機械的パターンに埋もれるという逆方向のリスクが生じるため、人間またはpostmortem workflowの明示的な判断に委ねる。

## 帰結

- queued Issueのキュー全体が、内容に基づいてloop-continuity/bug/feature/improvementへ分散し、重要な種別がimprovementに埋もれにくくなる。
- 既存の`agent:queued` 15件は、本トリアージ実装の適用後、Supervisorの次回reconcileで再分類される（複数カテゴリの集約と異なり、ゼロカテゴリのIssueのみが対象）。
- 自動分類の判断根拠（`reason=content selected=CATEGORY`）はIssueコメントに残り、誤りがあればqueued中にLabelを1つだけ残して再トリアージできる。
- incidentの自動誤判定という新たなリスクは、キーワード分類の対象からincidentを除外することで作らない。

## 対象外

- `flaky`修復Issueや`diagnosis`所見Issueなど、他モジュールが独自に`category:improvement`を付与する経路の変更（対象範囲外、必要なら別Issueで判断する）。
- incidentの自動検出・自動分類の実装（引き続き人間またはpostmortem workflowの判断に委ねる）。
