# 検証ハーネスポリシー

## 目的と優先関係

このポリシーは、変更ライフサイクルの早い段階で問題を検出しつつ、pushとmergeの前に完全な検証を保証するため、すべての変更に適用する。検証環境と共通入口は[開発環境ポリシー](development-environment.md)、テスト内容は[テストポリシー](testing.md)、CIの採否と外部サービスの利用は[費用ポリシー](cost.md)に従う。金銭的コストに関する判断では費用ポリシーを最優先し、CIの実行条件についてはこのポリシーをテストポリシーより優先する。それ以外は各ポリシーを同時に満たさなければならない。

## 共通入口と検証層

ローカルとCIは、同じリポジトリ内のコード化済み環境と同じ共通入口を使用する。共通入口から呼ぶ処理や依存をCI専用に分岐させてはならず、検証内容の追加・変更は共通入口へ反映する。このリポジトリの完全チェックとCIの共通入口は `devbox run --pure check` とする。

検証を次の層に分ける。

- **local fast check**: format、lint、静的解析、secret検査など、短時間で決定的に完了する検証。該当する検証をcommit前を含む可能な限り早いlocal hookから実行し、失敗時はcommitを停止する。
- **local full check**: 単体test、統合test、E2Eその他の時間がかかる検証を含む共通入口の全処理。変更対象のcommitに対してpush前に成功させ、失敗時はpushを停止する。pushに連動して実行する場合も、成功終了までpushを完了させてはならない。
- **CI**: public repositoryでpushおよびpull requestに対して共通入口を実行する独立した完全検証。対象commitの必須checkがすべて成功するまでmergeを停止する。

各検証は単独で再実行でき、以前の実行結果や実行順序に依存してはならない。失敗時は変更を先へ進めず、原因を修正して同じ入口を再実行する。hook bypass（`--no-verify` など）を通常運用として認めない。hook自体の障害でやむを得ず別経路を使う場合も、同一commitへの同等検証の成功、理由、実行環境、コマンド、結果を変更記録に残さなければpushまたはmergeしてはならない。

## CI採否とprivate repository例外

repositoryの可視性、GitHub Actionsの料金条件、利用枠を推測してはならない。GitHubが返す可視性と、適用時点の料金・請求設定を確認し、費用ポリシーが許容しない追加費用の可能性がない場合にだけCIを実行する。

public repositoryでは共通入口をCIで実行し、その成功をmerge gateとする。private repositoryでGitHub Actionsの追加費用を確実に排除できない場合は、費用ポリシーを優先してCIを実行せずにmergeしてよい。この例外では、merge対象commitそのものに対する `devbox run --pure check` のローカル成功を必須のmerge gateとし、commit SHA、固定環境の定義またはlock file、実行コマンド、終了結果、実行日時をIssueまたはPRへ記録する。記録したcommitとmerge対象が一致しなければ、完全チェックを再実行する。

可視性または料金条件を確認できない場合はCIを実行せず、かつprivate repository例外を推測で適用せず、pushまたはmergeを停止して確認可能な状態へ戻す。

## push・merge gateと証跡

未成功の変更をpushまたはmergeしてはならない。push gateは、対象commitに対するlocal full checkの成功とsecret検査の成功である。merge gateは、push gateの証跡、未解決のreview feedbackがないこと、およびpublic repositoryでは対象commitの必須CI成功、private repository例外では対象commitのローカル完全チェック成功である。修正commitを追加した場合は、そのcommitに対して各gateを再評価する。

検証証跡には少なくともcommit SHA、コード化済み環境を識別する定義またはlock file、共通入口のコマンド、成功または失敗の結果を含める。失敗も隠さず、修正と再実行の結果を関連付ける。CIのcheck URLまたはrun識別子がある場合は併記する。

## AI review

AI reviewを採用する場合は、費用ポリシーが許容する既存のCodex loginだけを使い、別途課金されるAPI keyや外部サービスを使用しない。秘密情報を入力へ含めず、対象commit、promptまたは再実行可能な手順、結果を記録する。AI reviewは決定的な自動テストの代替にせず、失敗、未完了、または重大な指摘がある間はpushまたはmergeを停止する。
