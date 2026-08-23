# v2 作業orchestration

この文書はv2構築時のagent分担、handoff、escalation、統合方法の正本である。Codexのcustom
subagentはbootstrap実装手段として使うが、task stateの正本にはしない。

## 1. 狙い

高価なモデルの context を実装 log で消費せず、判断の難しさに応じて仕事を routing する。会話上の親子関係ではなく、永続化された task state と work packet を役割間の契約にする。

## 2. 三つの責務

| Role | 主な責務 | 入力 | 必須 output | 想定 model |
| --- | --- | --- | --- | --- |
| Product Owner／Orchestrator | 課題の深掘り、優先度、task DAG、acceptance、不変条件、release 判断 | user problem、product state、要約 evidence | Problem Brief、task graph、完了／差戻し判断 | Sol |
| Tech Lead | 境界内の詳細設計、contract、影響範囲、検証設計、実装可能な分解 | Problem Brief、関連 architecture、対象 component | Design Packet、Work Order、review verdict | Terra |
| Implementer | 限定された変更、対象 test、evidence 採取 | implementation-ready Work Order と必要 file | patch、test evidence、発見事項 | Luna |

Sol は通常 raw source や test log を読まず、構造化された要約と例外だけを読む。Terra は repository 全体ではなく関連 contract と component を読む。Luna は Work Order で許可された境界を越えない。

## 3. 階層ではなく escalation graph

組織の比喩は有用だが、モデルを地位で固定しない。仕事の曖昧さと risk で routing する。

- Luna は acceptance、対象境界、検証入口のどれかが不明なら推測せず Terra へ返す。
- Terra は product trade-off、複数 boundary の再設計、不変条件の解釈が必要なら Sol へ返す。
- 明確で機械的な task は Terra を毎回往復せず Luna が連続処理できる。
- 高 risk な実装や原因不明の反復失敗は、実装自体を Terra または Sol 相当へ昇格できる。

## 4. Work Packet gate

Luna に渡す前に、packet が次を満たすことを schema validation する。

- problem と observable outcome
- scope／non-scope と変更可能 component
- 利用／変更する contract
- acceptance criteria と validation commands
- secrets、cost、destructive action の制約
- failure 時の停止条件と escalation destination

不足 packet を Luna に補完させない。曖昧さを安価なモデルへ押し付けると、修正 loop によって総 token と lead time が増えるためである。

## 5. 並列化と統合

Sol が task dependency DAG を作り、独立 task の Design Packet を Terra が並列に作れる。implementation-ready になった task は複数 Luna が一時 worktree で実行する。write-heavy な task は同一 component owner を同時に一つに制限し、coordinator が validation 後に v2 branch へ統合する。

## 6. 永続状態の最小構成

```text
.agents/v2/
  backlog/
  tasks/
  work-packets/
  decisions/
  evidence/
  releases/
```

各状態遷移は actor、入力 hash、output hash、次の owner、retry budget を記録する。prompt 全文や秘密情報は保存しない。進行中 agent の lease が失効すれば、同じ packet から別 agent が再開する。

## 7. Token budget の置き場所

- Sol: Problem Brief と task graph の作成・例外判断に budget を使う。
- Terra: task 単位で設計 budget を持ち、関連 file／contract だけを context に入れる。
- Luna: 一つの Work Order を一つの context で完結できる粒度にする。
- 失敗 log は正規化・要約して上位へ渡し、raw log は evidence storage に置く。
- 同じ失敗を再試行する前に packet を改訂し、無意味な token 消費を止める。

## 8. 起動とreviewの決定

- bootstrapからM2まではCodexのproject-scoped custom subagentを使う。M2でControl Planeのtask stateが成立した後は、同じpacket契約を製品schedulerから起動する。Codex threadはどちらの期間もcanonical stateにしない。
- Solは常駐させず、課題受付、task graph変更、escalation、milestone／release判定など重要なstate transitionで起動する。
- TerraはDesign Packetを作成し、通常はschema validationと実装後Evidenceで自己完結する。別Terraによるreviewは、security／state schema／external side effect／release boundaryの変更、または同一failureの反復時だけ起動する。
- Lunaの出力はcoordinatorがWork Orderのscope、diff、affected validation Evidenceに一致することを機械検査してからv2へ統合する。

## 9. Bootstrap期間の制約

Codex subagentの並列実行は速度向上の手段であり、無条件には使わない。read-heavyな独立調査は並列化し、write-heavyな作業はcomponentごとに同時writerを一つに制限する。各agentの終了前に結果をcanonical task stateとEvidence indexへcheckpointし、親threadの要約だけに依存しない。
