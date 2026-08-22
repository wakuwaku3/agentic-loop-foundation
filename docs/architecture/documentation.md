# 文書体系とmaintenance契約

更新日: 2026-08-22

## 1. 原則

- 利用者が現在使える機能はStable文書だけで理解できる
- Preview利用者はPreview文書だけで利用でき、Stableとの差分も分かる
- 現在仕様、設計理由、運用手順、生成referenceを混ぜない
- 同じ事実を複数の手書き文書へ複製しない
- 文書を実装と同じRelease Artifact、同じpromotion gateで扱う

## 2. 読者別の正本

```text
AGENTS.md
└─ 大原則（毎turn読む最小の不変条件）

docs/stable/                    # Stable利用者
├─ index.md
├─ getting-started.md
├─ problems-and-backlog.md
├─ runners-and-providers.md
├─ preview-and-stable.md
├─ control-and-recovery.md
└─ troubleshooting.md

docs/preview/                   # Preview利用者
├─ index.md                     # Preview全体の利用方法
├─ stable-diff.md               # Stableとの差分・既知問題・rollback
└─ ...                          # Stableと同じ情報architecture

docs/product/                   # 何を提供するか
├─ definition.md
└─ user-facing-spec.md

docs/architecture/              # 実装・運用担当
├─ overview.md
├─ domain-model.md
├─ failure-model.md
├─ security.md
├─ release-contract.md
└─ decisions/                   # ADR

docs/runbooks/                  # 異常時に実行する手順
├─ emergency-stop.md
├─ rollback.md
├─ credential-incident.md
└─ firestore-restore.md

contracts/                      # 機械可読な正本
├─ openapi/openapi-v1.yaml
├─ events/
├─ work-packet/
└─ release-contract/

generated/reference/            # contractsから生成。手編集しない
```

## 3. AGENTS.md

AGENTS.mdには大原則と、作業種別から正本へ到達する短いroutingだけを置く。

- 毎turn読む内容を大原則へ限定する
- 詳細な停止protocol、API、test commandを複製しない
- 作業に関係する仕様／architecture／runbookだけを読むようroutingする
- 大原則の変更には人間の明示承認が必要であることを記す

## 4. Stable／Preview文書

### Stable

- default URLで現在Stable versionの文書を表示する
- release noteの羅列ではなく現在形の完全仕様を書く
- UIの各画面から該当操作へcontext linkする
- rollback対象の過去Stable文書はversion URLで保持する

### Preview

- Preview tag URLでcandidate versionの文書を表示する
- Stable文書を読んだ前提にしない完全な操作説明を持つ
- `stable-diff.md`に追加、変更、廃止、migration、既知問題、rollbackをまとめる
- 未実証capabilityと昇格blockerを自動表示する

StableとPreviewで同じ章を手作業copyしない。sourceはversion branch／Release bundle内で一つ持ち、build時に
channel bannerと差分情報を組み合わせる。

## 5. Capabilityと文書の対応

Release Contractの各capabilityは次を参照する。

- Stable利用者文書anchor
- Preview利用者文書anchor
- UI route
- exercise implementation
- Evidence schema

文書anchorがないcapability、存在しないanchor、version不一致はpromotionを止める。

## 6. 自動maintenance

schemaから生成するもの:

- API endpoint／field reference
- Requirement／Increment／Release状態一覧
- Event type一覧
- Provider capability matrix
- configuration key reference
- CLIが残る場合のcommand reference

人間／Loopが説明を書くもの:

- 課題をどう登録するか
- 状態をどう解釈するか
- stop／rollbackをいつ使うか
- Stable／Previewの考え方
- failure時の判断と復旧
- 設計理由とtrade-off

generated referenceを説明文へcopyせずlinkする。

## 7. Change contract

利用者が観測できる挙動を変えるIncrementは、同じIncrementで次を行う。

1. user-facing specまたはRelease Contractを更新する
2. Preview利用者文書を更新する
3. capability exerciseを更新する
4. 必要ならmigration／rollback runbookを更新する
5. schema／UI／実装を変更する
6. docs drift checkとPreview実証を行う

文書だけを後続Requirementへ先送りしない。内部refactorで利用者挙動が変わらない場合は、利用者文書を
不要に書き換えずarchitecture／ADRだけを更新する。

## 8. ADR

ADRは採用したdecisionとtrade-offを残し、現在の操作仕様を説明しない。

- Context
- Decision
- Alternatives
- Consequences
- Revisit trigger

decisionが置換された場合、旧ADRを削除・書換えずsuperseded linkを付ける。現在architectureはoverviewへ
反映し、ADRを時系列に読まないと理解できない状態を作らない。

## 9. Verification

- Markdown lint、link、anchor、spelling
- docs内versionとRelease manifestの一致
- generated referenceのclean regeneration
- UI label／routeと文書の照合
- state／Provider一覧とschemaの照合
- code blockの安全なsmoke
- Stable docsからPreview-only機能への誤参照検出
- Preview diffにbreaking changeとrollbackがあることの検査
- 初見利用者personaによるAI review

AI review promptと結果は再現可能にするが、決定的checkの代替にはしない。

## 10. Ownership

Loopが通常の文書maintenanceを行い、人間reviewは不要とする。人間の明示承認が必要なのは大原則の
意味変更だけである。

利用者文書の品質は「書いたか」ではなく、Release Contract capabilityを利用者が文書だけから実行し、
期待結果と復旧方法を理解できるかで評価する。

## 11. v2 開発中の設計・作業文書

white-slate branchでは、長い会話を引き継ぎ媒体にせず、次をv2 branch自身にversion管理する。

- product／architecture／decision documents
- component ownershipとcontract dependency manifest
- task ledgerとprovider-neutral work packet
- validation evidenceのindexとrelease candidate manifest
- cutover／rollback runbook

task固有の一時メモとStable／Previewのuser-facing documentationは混在させない。task完了時に残すべき知識は正本へ統合し、一時メモは削除できる状態にする。
