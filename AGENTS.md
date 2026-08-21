# Agentic Loop

要求が満たされるまで、調査・変更・検証・修正を自律的に反復する。

- アプリケーションとこの開発ループへの要求を同等に扱い、必要な要求は自ら発見・分解してよい。
- 不変条件は要求より優先し、常に維持する。
- 要求と不変条件を検証できない限り、完了としない。
- 要求に応じて、このループ自身も進化させてよい。
- 変更は専用worktreeで行い、検証済みのPRを作成し、確認・修正・マージまで完遂する。

## 要求のルーティング

- 対話中のAgentが通常のbuild・変更要求を受けた場合、メインworktreeを直接編集せず、Issueキューがセットアップ済みでGitHub Issueのread/writeと登録後状態を検証できるなら `submit-requirement` のqueue-first intakeへ載せる。必要最小限のactive Issue確認で同一要求を再利用または作成し、要求実態に対応する排他的な `category:*` 1個と `agent:queued` を付け、queuedまたはrunningを確認して終了する。
- 利用者が「Issueを作って」など新規Issue作成を明示した場合は重複検索を省略する。ただし、既存Issueの再利用・重複確認も明示された場合と、コード診断・定期監査では検索する。
- `agent:running` Issueを専用worktreeで処理中のworkerは受付を再実行しない。代替Issueを作らず、調査・変更・検証・PR・checks・review・merge・cleanupを完遂する。
- 利用者が同期実行または直接実装を明示した場合は、その指示を優先してworker手順を実行する。
- `.claude/settings.json` のPreToolUseフックは、メインworktreeの追跡ファイルへの意図しない直接編集を利用者に浮上させる確認層であり、`deny` による禁止ではない。専用worker worktree、未追跡scratchpad、読み取り操作は対象外とする。
- Supervisorの停止だけをIssue受付の失敗にしない。停止中も検証済みのIssueをqueuedとして永続化し、登録済みであること、停止中であること、処理開始にはSupervisorの起動が必要なことを報告する。GitHub権限または登録後状態を検証できない場合は成功扱いせず、[Issueキュー運用](docs/operations/issue-queue.md)の安全なfallbackに従う。
- 読み取り専用の質問、診断、status確認、start・stopなどの運用コマンドはIssue化しない。
- `agent:cancelled`、`agent:superseded`、`agent:duplicate`、`agent:merged` は終端状態であり、自動再queueしない。取消・統合・再開は認可済みの `bin/agentic-loop dispose`／`resume` だけで実行する。
- `agent:parked` は終端状態ではなくopenの人間トリアージ待ちである（[ADR 0016](docs/decisions/0016-failure-park-not-close.md)）。リトライ予算を使い切っただけで要求が無効になったわけではないため、Supervisorのどの自動経路もparked Issueを再claimしない。再投入は認可済みの `bin/agentic-loop resume` だけで行う。

## 不変条件

- 秘密情報をリポジトリへ保存しない。
- 変更は再現可能な検証手段と、必要十分な文書を伴う。
- [外部環境コード化ポリシー](docs/policies/external-environment.md)に従い、依存・操作する外部環境のdesired state、適用、drift検出、検証、復旧および移行を再現可能に管理する。
- [開発環境ポリシー](docs/policies/development-environment.md)に従い、固定されたコード化済み環境で開発・検証する。
- [AIツール非依存ポリシー](docs/policies/ai-tool-neutrality.md)に従い、特定のAIコーディングツールにベンダーロックせず、環境変数で選択可能な差し替え可能プロバイダとして扱う。
- [テストポリシー](docs/policies/testing.md)に従い、すべての自動テストを共通入口から実行する。
- [検証ハーネスポリシー](docs/policies/validation-harness.md)に従い、変更ライフサイクルの各gateで共通入口による検証を完了する。
- [継続的デリバリーポリシー](docs/policies/continuous-delivery.md)に従い、最初の本番deployまたはbinary releaseと同じ変更で、mainへのmergeを起点とする再現可能なCD pipelineを実装する。
- 破壊的・不可逆または重大なコストを伴う判断は、実行前に明示的な承認を得る（機械的な強制点は[preflightゲート](docs/operations/preflight.md)を参照）。
- [費用ポリシー](docs/policies/cost.md)をすべての機能・設計・運用に適用し、許容されない金銭的コストを発生させない。
- [GitHub日本語運用ポリシー](docs/policies/github-language.md)に従い、Issue、PR、コメント、レビューは日本語で記述する。
- [文書ポリシー](docs/policies/documentation.md)に従い、文書の対象読者と責務境界を守り、同じ事実を複数の文書へ重複させない。
- [有限資源とスケーラビリティのポリシー](docs/policies/resource-scalability.md)に従い、処理量・外部I/O量・入力規模に対する増加率を設計と検証の対象にする。
- [ポストモーテムポリシー](docs/policies/postmortem.md)に従い、重大な事故・near miss・反復失敗・資源枯渇から学び、非難せず、action itemが検証済みになるまでポストモーテムを未完了として追跡する。
- GitHubのIssue本文・commentをfileから渡す場合は `gh --body-file PATH` を使う。`gh --body "@path"` は展開されず内容が失われる（[ADR 0030](docs/decisions/0030-lost-requirement-detection.md)）。`gh api` の型付き値には `-f` ではなく `-F` を使う。
