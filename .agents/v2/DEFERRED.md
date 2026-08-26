# v2 後回しリスト（nice to have / gate を塞がないもの）

2026-08-26 時点。**ここに載っているものは1つも v2 の完了条件ではない。** どれも
実測で見つかり、見つけた task が意図的に直さず記録した事実である。直す価値の順ではなく、
**測った場所が分かる順**に並べてある。

判断規則: **release gate（M0〜M9）のどれかを塞ぐなら required、塞がないならここ。**
「気になる」「一貫していない」は required の理由にならない。

---

## D1. 2つの storage adapter が同じ問いに違う**答え**を返す3箇所

V2-087 が課金の非対称を閉じる過程で実測し、記録して直さなかった。**task V2-094 として
登録済みだが、どの gate も塞いでいない。**

| | memory adapter | firestore adapter |
|---|---|---|
| `IncrementsForRequirements` に空の id list | **全件返す** | **0件返す** |
| `Outboxes` の ready 判定 | `OutboxAmbiguous` と `OutboxReconciling` も ready | `{pending, waiting, delivering}` のみ |
| `SaveIdempotency` の同一 replay | **write を stage する** | stage せず戻る |

課金側は3つとも firestore の query を仕様として寄せてある。**残っているのは答えの差だけ。**

V2-087 がこれを同じ commit で直さなかったのは正しい: 計数規則の一致を主張する test と
答えの変更を混ぜると、赤が出たときにどちらの主張が壊れたのか分からなくなる。

直すときの注意: `SaveIdempotency` は V2-087 以降**両 adapter が同じ規則で日次 write budget を
課金する**ので、replay で write する側は他方が使わない予算を食う。ここでは安い答えと
正しい答えが一致する見込みだが、**一致が都合がいいことを根拠にしてはいけない** — 呼び出し側が
どちらに依存しているかを測って決める。

## D2. `nextAction` が、親が work を認めない状態でも "run next increment" と言い続ける

`internal/application/readmodels.go:309-332`（判定は :324-325）。V2-089 の設計が実測。
owner console の表示だけに効く。V2-090 が同じ file を持つので、直すなら V2-090 と同じ波で。

## D3. product doc の遷移表が domain より**2セル広い**

`docs/architecture/domain-model.md:268`（needs-input→active）と `:271`（evaluating→active）は
`internal/domain/model.go:486` が認めない。V2-089 が**表を典拠にできない**と判定した根拠でもある
（表に従うと needs-input からの claim を許すことになり、出荷済みの V2-065 拒否を壊す）。

**doc を直すか domain を広げるかは同じ問いではない。** needs-input は「表に active が
あっても claim は許されない」という product 側の反例として既に存在するので、
**doc 側を狭めるのが筋**に見えるが、`evaluating` については誰も測っていない。

## D4. 429 分岐が wall clock を読む

`internal/api/api.go:1176` の `nextUTCMidnight(time.Now())`（Retry-After header 用）。
V2-088 が触った関数の中にある既存の決定性の漏れで、V2-088 は自分の recorder の
clock 規則だけを満たして**これは据え置いた**。injected clock に寄せれば消える。

## D5. `X-Correlation-ID` が無制限長で response に echo される

`internal/api/api.go:55-60`。caller が供給した値をそのまま response header と body に返す。
V2-088 は**自分の記録側だけ** 256 byte に bound したので、記録は膨らませられないが
response は膨らませられる。

## D6. openapi が 4 route で `500` も `400` も宣言していない

`contracts/openapi/openapi-v1.yaml`。V2-083 の finding（dp-v2-083 d11a）。
`domainError` を呼ぶ route のうち4本。V2-083 が default を 500 に反転したので、
**宣言と実挙動の差が広がった**状態。

## D7. `ErrInvalidTransition` と `ErrDuplicateRequest` が 400 のまま

V2-083 が意図的にそう据えた（dp-v2-083 d9）。状態機械の拒否は 409 が妥当という議論はあるが、
**pin table 202行がこの status を固定している**ので、動かすなら pin table の更新が伴う。
「一貫性」だけを理由に動かすと、V2-083 が買った回帰検知が薄くなる。

## D8. `ForbiddenPatternMatches` / `SecretPatternMatches` が test からしか呼ばれない

`internal/provider`。呼び出し元は `internal/provider/fixtures_test.go:148, :151, :173, :176` と
V2-088 の `operator_record_test.go` の cross-check だけ。non-test import を作ると
`ci/components.json` の編集が必要になるため、どの task も踏み込まなかった。

## D9. どちらの adapter にも delete 操作が無い

したがって settled usage の `deletes` は**常に 0**。V2-087 が記録した事実で、defect ではない。
delete を実装する日まで、`deletes` の 0 は「測っていない」ではなく「無い」を意味する。

---

## 解決済み — 追いかけないこと

- **`internal/api/api_test.go` の gofmt**: V2-087 が「base commit の時点で既に gofmt-clean でない、
  つまり `make lint` は赤」と報告したが、**統合後の tree で実測すると clean** で、
  `make check` も緑である。あれは worktree 固有の状態だった。
