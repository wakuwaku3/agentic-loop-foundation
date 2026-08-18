# このリポジトリの開発

このリポジトリの変更は、原則としてAgentic Loop（AI）が調査・実装・検証・PR作成・mergeまでを自律的に行う（[AGENTS.md](../AGENTS.md)）。人間の開発者は、AIが代われない次の4つの責務だけを担う。実装の詳細や検証規則はここには書かず、それぞれの正本にリンクする。

## 要求の伝え方

達成したいことを自然言語で入力する。書き方の詳細は [README.mdの「要求を出す」](../README.md#要求を出す)を参照する。受け入れ条件は、READMEだけを読んだ基本利用者でも判断できる粒度で書くと、queued後のトリアージが安定する。要求に対応するIssueへ `agentic-loop:scope` markerが付くことがあるが、これは変更競合の予防のための機械的な補助情報であり、人間が書く必要はない（詳細は [Issueキュー運用](operations/issue-queue.md#変更競合の予防)）。

## 意思決定・承認

破壊的・不可逆または重大なcostを伴う変更は、実行前に人間の承認を必要とする（[preflightゲート](operations/preflight.md)）。承認が必要な場合、対象Issueは `agent:needs-input` となり、`bin/agentic-loop preflight ISSUE --approve --token TOKEN` で承認するまで処理が進まない。要求の優先度変更（`bin/agentic-loop priority`）、実行の一時停止・再開・打ち切り（`pause`/`resume`/`abort`）、要求の終了・統合（`dispose`）も、write/maintain/admin権限を持つ人間の判断を必要とする。手順は [Issueキュー運用](operations/issue-queue.md#認可済みの終了統合)を参照する。

## レビュー

Agentic LoopはPR本文に `agentic-loop:traceability` recordを記録し、受け入れ条件がどのcommit・checkで満たされたかを追跡可能にする（[要求・変更・検証のトレーサビリティ](operations/traceability.md)）。人間のレビューは、このrecordとGitHubのcheck結果を確認し、要求と実装の対応を判断する作業に限定できる。`bin/agentic-loop trace ISSUE` で対応PRの評価結果を、`bin/agentic-loop trace --audit` でrecordが不整合なIssueを確認できる。

## 例外時の手動確認・復旧

Agentic Loopが停止・停滞した場合、まず `bin/agentic-loop doctor` で環境健全性を診断する（[Issueキュー運用](operations/issue-queue.md#事前診断)）。個別Issueの状況は `bin/agentic-loop status` で確認する。復旧が必要な典型例と対応は次のとおりである。

- 認証・権限・環境の不整合: `doctor` の指示に従って `setup`、`start`、install/upgradeの再実行で復旧する。
- 想定外の停止・再試行が続くIssue: `pause`/`resume`/`abort` で実行制御し、必要なら `dispose` で終了する（[Issueキュー運用](operations/issue-queue.md#実行の一時停止再開打ち切りadr-0019)）。
- Foundation自体の更新: `bin/agentic-loop upgrade`（既定はdry-run）で差分を確認してから `--apply` する（[導入済みFoundationのupgrade](operations/upgrade.md)）。
- 手元での検証再現: [開発環境ポリシー](policies/development-environment.md)が定める共通入口を使う。個別のコマンドや検証手順の詳細はそのポリシーと [Issueキュー運用](operations/issue-queue.md)を正本とし、ここには複製しない。
