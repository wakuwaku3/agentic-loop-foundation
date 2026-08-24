# Preview・Stable・利用者文書のrelease契約

更新日: 2026-08-22

## 1. 目的

自動testだけでは確認できない外部接続を含め、Stableで提供する全機能をPreview実環境で実証する。
利用者文書も実装と同じrelease artifactとして扱い、実装だけが先にStableになることを防ぐ。

## 2. Release Contract

各Repositoryはversion管理されたRelease Contractを持つ。これはStable候補を昇格させるために必要な
利用者向けcapabilityと実証方法の唯一の一覧である。

各capabilityは少なくとも次を宣言する。

- 利用者が行う操作または発生させる入力
- 利用者が観測すべき結果
- Preview実環境での実行方法
- 必要な外部system、AI Provider、credential scope
- 成功、失敗、rollbackの観測条件
- 証跡の保存先と有効期間
- 対応するStable／Preview利用者文書

内部functionやtest caseの一覧をcapability一覧の代わりにしない。

## 3. Preview実証

Stable候補は、Release Contractにある全capabilityを、その候補versionが動くPreview実環境で確認する。

- 変更されたcapabilityだけでなく、Stableとして提供する全capabilityを対象にする
- fake、stub、契約testは早期feedbackに使うが、実稼働確認を置き換えない
- 外部system依存機能は対象systemへ実際に接続する
- Provider非依存機能は、Release Contractで定めた代表Providerによる実行を確認する（代表Providerはcapability declaration setのrepresentative_providerで宣言する）
- Provider依存機能は、影響する各Providerを実際に利用する
- Codex依存変更はCodex、Claude依存変更はClaude、opencode依存変更はopencodeで確認する
- 共通Provider adapterやhandoff契約の変更はCodex、Claude、opencodeすべてで確認する
- 実行不能なcapabilityが一つでもあれば、理由にかかわらずStableへ昇格しない

費用やquotaを理由に確認を省略しない。上限内で実証できるよう頻度と入力を設計し、上限内で実行
できない候補はPreviewに留める。

## 4. 昇格

次を同じrelease versionに対して満たしたときだけStableへ自動昇格する。

1. 決定的な自動testと静的検証が成功した
2. Release Contractの全capabilityがPreview実環境で成功した
3. 対象となる実外部systemとAI Providerで成功した
4. stop、rollback、Stableによる再開を確認した
5. 未解決の重大な障害、秘密漏洩、許可されない費用がない
6. Preview利用者文書が候補versionの全機能を説明している
7. Stableとの差分、migration、rollback方法が説明されている
8. 実装、schema、migration、設定、利用者文書を同じversionとして昇格できる

昇格後にRequirementを完了へ移し、対応するStable文書を既定表示にする。

## 5. 利用者文書

### Stable文書

現在のStableを初めて使う利用者が、Previewや内部実装を知らなくても操作・制約・復旧方法を理解
できる完全な文書とする。過去の変更履歴だけで現在仕様を説明しない。

### Preview文書

現在のPreviewを単独で利用できる完全な文書とし、加えて次を明示する。

- 対応するPreview version
- Stableから追加、変更、廃止される利用者向け機能
- 既知の問題と影響範囲
- Stableへ戻す方法
- Stable昇格に不足している実証

### 文書のrelease

- capabilityを変更するIncrementは、同じPreview versionの文書も変更する
- 文書と実挙動のdriftをPreview gateで検出する
- Stable昇格時にPreview文書をStableの現在仕様へ統合し、Preview固有の警告を取り除く
- rollback対象versionの利用者文書を、rollback windowが閉じるまで保持する
- 同じ事実を複数文書へ手作業で複製せず、現在仕様と差分の責務を分ける

## 6. Versionとrouting

- Preview routingの単位はRepositoryとする
- Repositoryは一度に一つのLoop channelで新規Incrementを処理する
- StableとPreviewはcanonical stateを共有するが、実行versionと証跡を全eventへ記録する
- 複数RepositoryにまたがるIncrementは、対象Repositoryが同じchannelと互換contractを使える場合だけ開始する
- Release Contract、実装、利用者文書には同じrelease versionを付ける

## 7. 秘密の扱い

- 実Provider確認に使うcredentialをtest fixture、証跡、文書、canonical stateへ保存しない
- 証跡にはProvider、version、capability、時刻、結果、消費量だけを残し、promptやraw応答を既定で保存しない
- 実証processには対象capabilityに必要な最小scopeのcredentialだけを実行時に渡す
- leak検知時は昇格を止め、影響credentialを失効・交換し、安全確認後に全capabilityを再実証する
