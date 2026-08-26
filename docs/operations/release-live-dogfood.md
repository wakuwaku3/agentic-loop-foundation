# preview-local dogfoodの手順（Release Contract capabilityの実測）

このFoundation Repository自身をPreview対象として、環境class `preview-local`
（owner実機・実process・実CLI・Firestore emulator）でRelease Contractの
12 capabilityを1件ずつ実測する手順です。GCPへdeployせず、GCP projectにも
IAPにも実Firestoreにも触らず、forgeにもremoteにも接続しません。

対象は `internal/api/live_dogfood_test.go` の
`TestFoundationPreviewLocalDogfood` です。`make check` はこれを実行しません。

## gate変数

- `AGENTIC_LOOP_LIVE_DOGFOOD=1` … 未設定なら、満たされていない条件を記録して
  skipします。したがって既定の検証はprocessを1つも起動せず、Provider invocationを
  1回も行いません。
- `AGENTIC_LOOP_LIVE_PROVIDER=1` … 実claude CLIを子process groupとして起動する
  sub-exerciseを有効にします。`AGENTIC_LOOP_LIVE_DOGFOOD=1` を設定したのに
  こちらを設定していない場合はskipではなくFatalです。
- `FIRESTORE_EMULATOR_HOST` … `scripts/firestore-emulator.sh` が設定します。
  gateを立てたのにこれが空、V2-022のprovider-preflight recordが無い・schema不正・
  台帳検査に落ちる、あるいはledger pathが書けない場合は、いずれもskipではなくFatalです。

## 実行するargv（timeoutを含む）

```sh
devbox run --pure \
  -e AGENTIC_LOOP_LIVE_DOGFOOD=1 \
  -e AGENTIC_LOOP_LIVE_PROVIDER=1 \
  -e HOME=/home/takushi \
  -- scripts/firestore-emulator.sh \
  go test -count=1 -v -timeout 3600s \
  -run TestFoundationPreviewLocalDogfood ./internal/api
```

`devbox run --pure` は環境を落とすので `HOME` は `-e` で渡さなければなりません
（claude CLIは `$HOME` 配下の自分のcredential storeを読みます）。working
directoryはrepository rootです。process全体のtimeoutは3600秒です。

安定性の確認は再試行ではありません。**同一commitで、別processとして2回実行し、
両方のverdictを記録します。** capabilityのcheckをpassedと記録できるのは2回とも
passingを観測した場合だけです。1つでも食い違ったら、そのcheckをfailedとして
両方の観測を転記し、当該capabilityの証跡idは書かず、停止してescalateします。
3回目を実行して同点を崩してはなりません。

## 記録する識別子

`docs/architecture/release-contract.md` 3節が要求する識別子を、実行と同じ
sessionで取得してevidenceに転記します。

- 環境class（`preview-local`）
- machine識別子（gethostnameの値と `uname -sr`）
- emulator名、`firebase --version` の出力、project（`agentic-loop-test`）、
  実際に使った `127.0.0.1:<port>`
- claude CLIの絶対pathと `--version` の出力
- Go toolchainのversion
- 実invocationごとにCLIが発行した session_id

prompt・生応答・応答の抜粋・credential値は、test file・文書・evidence・commit
messageのいずれにも記録しません。使用量は input・output・cache-read・
cache-creation の各countで報告します。

## このgradeが証明できない4条件（初回deploy gate D1）

次の4つはこの手順の対象外であり、ここで主張してはなりません。

1. IAPによるowner認証境界
2. idle時のscale-to-zero
3. 実Firestoreの権限と競合
4. 承認済みplan digestを経るdeploy経路

宣言する外部systemに `Google Cloud Run` を含むcapabilityは、localの挙動を
どれだけよく観測しても証跡idを受け取れません。

## capabilityの証跡id付与規則

証跡idを `contracts/release-contract/foundation.json` に書けるのは、次の2つが
**両方とも実測で**成り立つcapabilityだけです。

1. `contracts/release-contract/foundation-capabilities.json` が宣言する
   `outcome_conditions.success` の文が、実行中にすべて観測できた。
2. 同じ宣言の `external_dependencies.systems` と `.providers` に挙がっている
   systemとproviderのすべてに、実際に接続した。

満たさない場合の正しい記録は、failedのcheckと空の `evidence_ids` です。宣言を
結果に合わせて書き換えることは、契約を弱めることであり禁止です。capability宣言
（`preview_exercise`・`outcome_conditions`・`external_dependencies`）は1 byteも
変えてはなりません。

Stableへの昇格はこの手順では行いません。`docs/stable/index.md` の
`Stable release: none` は不変です。

## escalation一覧（この手順で決めてはならないもの）

- E22-1: Repository登録route・登録command・Git/forge clientはHEADに存在する。
  V2-022発行時点の「treeに無い」測定は偽。
- E22-2: needs-inputのcommand・route・詳細fieldは存在するが、実surfaceで作られた
  Requirementはneeds-inputへ遷移できるstatusに到達できない。
- E22-3: `internal/release`はapplication層から使われ `GET /v1/release/state` も
  存在するが、`cmd/control-plane` がReleaseObserverを組み立てないため稼働processは
  503を返す。
- E22-4: Provider registryのread routeは存在する。
- E22-5: schedulerは配線済みで、配分上限と待機理由と枯渇が報告される。
- E22-6: 出荷される `cmd/runner` は `--fake` なしでは起動せず、control planeへの
  配線が無い。
- E22-7: Backlogを関連Repositoryで絞り込めない。
- E22-8: control read modelに対象Runner・process・lease・新規副作用可否が無い。
- E22-9: この実測fileのimportは `api` componentの宣言依存edgeを超えるため、
  selective CIはこれを選択しない。
- E22-10: owner consoleは文書routeを提供しない。
- E22-11: Firestoreに対してBacklogを2ページ目以降へ進められない（cursorを渡すと
  routeが400を返す）。

## 費用台帳

実claude invocationはV2-022のprovider-preflight record
（`.agents/v2/provider-preflight/V2-022-provider-live-claude-dogfood.json`）の
下でのみ行い、台帳はrepository外の `limits.ledger_path` に置きます。
recordのしきい値は暴走検知であって課金上限ではありません。しきい値に到達すること、
または単一invocationの異常しきい値を超えて `halted` が立つことは**点検のための
停止**であり、成功でもcapabilityの失敗でもありません。到達したら、recordの
`limits` を書き換えず、台帳fileを編集も初期化もせず、`halted` を消さず、当該
capabilityの証跡idを書かずに停止します。回復手段は、新しいidの（必要なら新しい
ledger pathの）owner発行recordを作ることだけです。
