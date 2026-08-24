# GCP 運用 runbook

更新日: 2026-08-22

このリポジトリの GCP desired state は `infra/` の OpenTofu です。対象は
既存 project、既定 Firestore database、既存 Artifact Registry の digest image、IAP
付き Cloud Run v2 一つだけです。外部 HTTP(S) Load Balancer、named Firestore database、
Artifact Registry repository は作成しません。

## 料金・制限の前提

確認日 2026-08-22。`us-central1` の Cloud Run request-based billing は monthly free tier
（180,000 vCPU-seconds、360,000 GiB-seconds、2,000,000 requests）を基準とする。pinned
cpu=0.08/memory=512Mi では 50% 上限は 360,000 instance-seconds/month（CPU 側の理論上限は
1,125,000）、requests は 1,000,000/month。Cloud Scheduler は M2 では 0 job
（`enable_reconcile_scheduler=false`）に固定する。
Firestore の既定 database は 1 GiB、50,000 reads/day、20,000 writes/day、20,000 deletes/day
が無料で、named database は無料枠の対象外である。roadmap M2 の完了条件は無料枠の 50% を
超えないことなので、アプリは毎日 25,000 reads、10,000 writes、10,000 deletes（無料枠の
50%）を hard reservation として日次 aggregate と 32 個の監査用 accounting bucket に記録する
（bucket は同一 document 内の内訳であり、Firestore contention shard ではない）。stored
bytes は日次 flow ではなく level なので UTC 日次リセットの対象外で、`internal/quota.
StorageLevel` が 268,435,456 bytes（document payload、index storage 2x overhead を仮定して
1 GiB の 50% の半分）を 536,870,912 bytes（1 GiB の 50%）に対して enforce する。

read transaction は最大 bounded query 1001 reads＋quota document read/write（合計
6,001 reads）を、mutation は最大 32 reads / 16 writes を mutation 前に予約し、超過は
fail closed する。6,001/25,000 だと 1 日 4 read transaction しか通らず実用に耐えないため、
その保守的な worst-case 予約はそのまま維持しつつ、実際にトランザクションが読んだ document
数（unit-of-work cache のサイズ）とステージした write/delete 数を使って、flush 直前
（`internal/store/firestore.unit.flush`）に実測値へ true-up する。true-up は予約を絶対に
超えて増やさず、0 未満にもしない。measured: single bounded page（1,002 reads）まで
true-up すると 1 日 24 read transaction まで通る（`internal/quota` のテストで実測、
`go test -count=1 ./internal/quota -run TestMeasuredDailyCapacityAtFiftyPercentCeiling -v`）。
予約超過は `quota.ErrOverBudget` となり、`internal/api` で HTTP 429 `quota_exhausted`
（`Retry-After` は次の UTC 深夜）にマップされる。400 invalid_request にはならない。

一次資料:

- [Cloud Run pricing](https://cloud.google.com/run/pricing)
- [Firestore pricing](https://cloud.google.com/firestore/pricing)
- [Cloud Run direct IAP](https://cloud.google.com/run/docs/authenticating/identity-aware-proxy)
- [google_cloud_run_v2_service provider reference](https://registry.terraform.io/providers/hashicorp/google/7.45.0/docs/resources/cloud_run_v2_service)
- [OpenTofu language](https://opentofu.org/docs/language/)

## 初回 plan/apply

認証・project は実行環境から明示的に与える。OpenTofu は 1.12.5、Google provider は
7.45.0 に固定する。secret、サービスアカウント鍵、tfstate は
リポジトリへ保存しない。

state は OpenTofu state 用の private/versioned Cloud Storage bucket（`infra/versions.tf`
の partial `backend "gcs" {}`）を先に用意し、bucket 名を `TF_STATE_BUCKET` として渡す。
`infra-plan.sh`/`infra-drift.sh` は `TF_STATE_BUCKET` が空だと fail closed する。
`infra-validate.sh` は `tofu init -backend=false` のままなのでローカル閉域では bucket
不要。

```sh
cp infra/terraform.tfvars.example /tmp/agentic-loop.tfvars
# project_id、既存 repository、64桁 image_digest、IAP owner を /tmp で編集
TFVARS_FILE=/tmp/agentic-loop.tfvars TF_STATE_BUCKET=<bucket> devbox run --pure -- ./scripts/infra-plan.sh
```

plan は `infra/build/infra.tfplan` に保存され、`infra/build/infra.plan.json`
（`tofu show -json` の出力）とその sha256 が同時に出力される。この sha256 が preflight
record（`contracts/schemas/deployment-preflight.json`、`.agents/v2/preflight/`）の
`target.plan_digest` になり、人間の承認はこの digest に対して行う。apply は
**承認された保存済み plan をそのまま apply する**のであって再 plan しない
（`tofu apply -input=false build/infra.tfplan`）。deploy workflow の apply step は
実行前に「plan step が今回生成した plan の sha256」と「承認済み preflight record の
`target.plan_digest`」が一致することを確認し、不一致または record 欠落なら fail
closed する。これは、承認された対象と実行される対象が別物になり得る
`tofu apply -auto-approve -var-file=...`（再 plan してしまう）を避けるためである。

この環境では `gcloud auth list` と project を確認できない場合があるため、ローカルでの
cloud apply は行わない。`tofu plan -refresh-only` は drift 検知であり変更を適用しない。

## drift、rollback、destroy

```sh
TFVARS_FILE=/tmp/agentic-loop.tfvars TF_STATE_BUCKET=<bucket> devbox run --pure -- ./scripts/infra-drift.sh
```

`infra-drift.sh` は `tofu plan -refresh-only -detailed-exitcode` を使う: exit 0 は
drift なし、exit 2 は drift ありでスクリプト自体が非 0 で終了する（以前は plan を印字する
だけで drift を無視していた）。

image digest の rollback は revision rollback であり resource 削除ではない: 既知の正常
digest へ `image_digest` を戻し、新しい plan を作り直し、その新しい plan_digest に対して
改めて人間の承認を得てから、その承認済み plan を apply する。rollback 完了条件は
(1) 直前の digest で build された revision が traffic 100% を受けている、(2)
`tofu plan -refresh-only -detailed-exitcode` が exit 0、(3) IAP 経由の `/healthz` が
200、(4) rollback 前後で canonical Firestore document の content digest が一致、の
4 つ全て。Cloud Run と Firestore は `prevent_destroy`/`deletion_protection` で保護され、
destroy は通常の経路では拒否される。`google_firestore_database` は
`deletion_policy = ABANDON` でもあるため、OpenTofu が project を再び空の状態へ戻すことは
不可能であり、state を失った場合の再 apply は `tofu import` が必要で、`tofu state rm` で
保護を迂回してはならない。緊急削除は別途明示承認、state backup、影響範囲の記録を先に
完了する。
# Optional reconcile Scheduler

Cloud Scheduler is disabled by default (`enable_reconcile_scheduler=false`).
Google's current pricing page states three jobs per billing account are free,
then `$0.10/job/31 days`; the account-level free-tier usage is not observable
from this repository, so enabling the job requires
`reconcile_cost_preflight_approved=true` and an existing custom IAP audience.
Checked 2026-08-22: [Cloud Scheduler pricing](https://cloud.google.com/scheduler/pricing).

When explicitly enabled, IaC creates only the dedicated
`agentic-loop-reconciler` service account, grants it
`roles/iap.httpsResourceAccessor`, and configures a POST OIDC target to
`/internal/reconcile`. Cloud Run's direct-IAP service-agent invoker binding is
unchanged; the scheduler account does not receive direct `roles/run.invoker`.
`RECONCILE_IDENTITY` is injected from the service-account email and the
application accepts only that normalized IAP assertion. The OIDC audience must
be a custom IAP OAuth audience because Google-managed default IAP clients are
not assumed programmatic-safe. See [Cloud Scheduler HTTP target auth](https://cloud.google.com/scheduler/docs/http-target-auth)
and [IAP programmatic authentication](https://docs.cloud.google.com/iap/docs/authentication-howto).
