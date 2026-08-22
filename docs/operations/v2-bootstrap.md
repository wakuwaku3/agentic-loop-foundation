# v2実行preflight

更新日: 2026-08-22

この文書はwhite-slate v2を開始する直前の観測事実、保護方法、最初のtask graphを定める。実行時には外部状態を再観測し、値が変わっていればこのsnapshotを盲目的に使わない。

## 1. 観測snapshot

| 項目 | 観測値 |
| --- | --- |
| v1 baseline候補 | `main`／`origin/main` = `56ee09710f049a3855ce0e2a931c6ea51939b2f9` |
| Supervisor | stopped、Running Issue 0、claim-paused |
| legacy backlog | queue 27件 |
| open PR | 4件。v2 bootstrapの前提にはしない |
| main protection | GitHub API上は未設定 |
| main worktree | 利用者所有の`.agentic-loop/manifest.json`変更あり |
| existing worktree | 複数あり。v2開始時に削除・再利用しない |
| rearchitecture docs | `tmp/rearch/`、gitignore対象 |

main worktreeのdirty stateはv2 branch pointへ入らない。`.agentic-loop/manifest.json`をstash、restore、commit、破棄しない。v2は別worktreeで`origin/main`の検証済みcommitから作る。

## 2. v1 freezeの扱い

- Supervisorの停止とclaim-pausedを維持し、新しいv1要求をclaimしない。
- 既存Issue、PR、worktreeを一括削除・close・mergeしない。
- v1のStable運用を維持するためのemergency fixだけを個別判断する。
- open IssueとPRはM9のone-time importで課題として再評価し、v2へsource commitを機械移植しない。
- branch point確定後のv1 emergency fixは、解決した課題とfailure fixtureだけをv2へ再評価する。

## 3. 保護refとworktree

実行直前に`origin/main`をfetchし、local `main`との差異、署名対象commit、稼働worker 0を再確認する。推奨する識別子は次とする。

| 用途 | 値 |
| --- | --- |
| v1 immutable recovery tag | `v1-pre-v2-20260822`。実行時HEADが変われば日付と対象hashを更新 |
| v2 integration branch | `v2` |
| v2 coordinator worktree | `/home/takushi/repos/agentic-loop-foundation-v2` |
| task branch | `v2/task/<task-id>` |
| task worktree root | `/home/takushi/repos/agentic-loop-foundation-v2-worktrees/` |

recovery tagはlocal作成後に対象hashを検証し、remoteへpushして単一machine障害から保護する。`v2`は通常branchとしてv1 commitを親に持つ。orphan branchにはしない。

## 4. GitHub protection

現在の`main`は未保護なので、v2開始前に少なくとも次を設定する。

- force pushとbranch deletionを禁止する
- required linear historyは、最終一括merge方式と衝突するため必須にしない
- v2開発中のmain direct writeはv1 emergency operatorだけに限定する
- v2 branchはcoordinatorによる直接checkpoint統合を許可し、人間PR reviewを必須にしない
- 最終main置換はM9 gateと明示的cutover commandだけに許可する

GitHub branch protectionの変更とremote tag／branch作成は外部状態変更なので、実行前に人間の承認対象をまとめて提示する。

## 5. Bootstrap transaction

白紙化を一つの巨大で不透明な変更にせず、復旧可能なcheckpointへ分ける。

1. `origin/main`をfetchし、baseline hash、Supervisor停止、Running 0、dirty main非接触を再確認する。
2. baselineへannotated recovery tagを作り、hashをread-backしてremoteへ保存する。
3. baselineから`v2` branchと専用coordinator worktreeを作る。
4. v2 worktreeだけでtracked treeを白紙化する。main worktreeと既存worktreeには触れない。
5. 同じbootstrap checkpointへ大原則、product／architecture文書、Devbox、secret guard、最小Go module、task ledger schemaを置く。
6. secret scan、document link、environment reproduction、empty-domain validationを実行する。
7. 成功Evidenceと次taskをcommitし、`v2`をremoteへpushする。

手順4だけが成功して5以降が失敗した場合、未完成treeを公開せず、v2 worktree内で修正する。baselineのv1 tagとmainは変更しないため、v1運用には影響しない。

## 6. 最初のtask graph

```text
V2-000 preflight approval / freeze observation                 [Sol]
  └─ V2-001 recovery refs + isolated v2 worktree               [Terra → Luna]
      └─ V2-002 white-slate bootstrap + tracked design baseline [Terra → Luna]
          ├─ V2-003 task/work/evidence schemas                 [Terra → Luna]
          ├─ V2-004 Devbox + Go scaffold + common entrypoint   [Terra → Luna]
          └─ V2-005 component DAG + selective CI bootstrap     [Terra → Luna]
               └─ V2-006 M0 aggregate evidence + checkpoint    [Sol]
```

V2-001とV2-002はrepository構造と保護点を変更するため直列にする。V2-003〜V2-005はV2-002のcontract確定後、変更componentを分離できる場合だけ並列化する。V2-006でM0のacceptanceを評価し、不足があれば新しいbounded taskへ戻す。

## 7. Agent起動contract

- SolはV2-000とV2-006を所有し、raw実装logではなくtask stateとEvidence indexだけを読む。
- Terraは各taskのDesign Packetを作り、変更可能path、contract、validation、rollbackを確定する。
- Lunaは一つのtask worktreeで一つのWork Orderだけを実行する。
- v2 coordinatorだけがtask commitを`v2`へ統合する。agent同士が同じworktreeを共有しない。
- agent終了時は、成功・失敗を問わずtask ledger、Evidence参照、次の安全なactionをcommit可能な形で残す。

## 8. Go／no-go gate

次をすべて満たすまで白紙化を開始しない。

- 人間がv1 freeze、remote recovery tag、v2 branch、main protection変更を明示承認した
- `origin/main`と採用baseline hashが一致する
- Supervisor停止、Running worker 0、新規claim停止を再確認した
- v2 worktree pathが存在せず、既存worktreeと重複しない
- main worktreeのdirty fileを操作対象から除外した
- recovery tagがbaselineを指し、remoteからread-backできる
- bootstrap Work Orderに削除対象と初期復元対象の完全なmanifestがある
