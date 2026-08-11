# Agentic Loop

要求が満たされるまで、調査・変更・検証・修正を自律的に反復する。

- アプリケーションとこの開発ループへの要求を同等に扱い、必要な要求は自ら発見・分解してよい。
- 不変条件は要求より優先し、常に維持する。
- 要求と不変条件を検証できない限り、完了としない。
- 要求に応じて、このループ自身も進化させてよい。
- 変更は専用worktreeで行い、検証済みのPRを作成し、確認・修正・マージまで完遂する。

## 要求のルーティング

- 対話中のAgentが通常のbuild・変更要求を受けた場合、Issueキューがセットアップ済みでSupervisorが正常なら直接実装しない。同一要求を検索し、重複を作らずIssueを再利用または作成して、要求実態に対応する排他的な `category:*` 1個と `agent:queued` を付け、queuedまたはrunningを確認して終了する。
- `agent:running` Issueを専用worktreeで処理中のworkerは受付を再実行しない。代替Issueを作らず、調査・変更・検証・PR・checks・review・merge・cleanupを完遂する。
- 利用者が同期実行または直接実装を明示した場合は、その指示を優先してworker手順を実行する。
- キューまたはSupervisorを利用できない場合は状態を明示し、[Issueキュー運用](docs/operations/issue-queue.md)の安全なfallbackに従う。
- 読み取り専用の質問、診断、status確認、start・stopなどの運用コマンドはIssue化しない。

## 不変条件

- 秘密情報をリポジトリへ保存しない。
- 変更は再現可能な検証手段と、必要十分な文書を伴う。
- [外部環境コード化ポリシー](docs/policies/external-environment.md)に従い、依存・操作する外部環境のdesired state、適用、drift検出、検証、復旧および移行を再現可能に管理する。
- [開発環境ポリシー](docs/policies/development-environment.md)に従い、固定されたコード化済み環境で開発・検証する。
- [テストポリシー](docs/policies/testing.md)に従い、すべての自動テストを共通入口から実行する。
- [検証ハーネスポリシー](docs/policies/validation-harness.md)に従い、変更ライフサイクルの各gateで共通入口による検証を完了する。
- 破壊的・不可逆または重大なコストを伴う判断は、実行前に明示的な承認を得る。
- [費用ポリシー](docs/policies/cost.md)をすべての機能・設計・運用に適用し、許容されない金銭的コストを発生させない。
- [GitHub日本語運用ポリシー](docs/policies/github-language.md)に従い、Issue、PR、コメント、レビューは日本語で記述する。
