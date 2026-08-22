# GCP 運用 runbook

更新日: 2026-08-22

このリポジトリの GCP desired state は `infra/` の OpenTofu です。対象は
既存 project、既定 Firestore database、既存 Artifact Registry の digest image、IAP
付き Cloud Run v2 一つだけです。外部 HTTP(S) Load Balancer、named Firestore database、
Artifact Registry repository は作成しません。

## 料金・制限の前提

確認日 2026-08-22。`us-central1` の Cloud Run request-based billing は monthly free tier
（180,000 vCPU-seconds、360,000 GiB-seconds、2,000,000 requests）を基準とする。
Firestore の既定 database は 1 GiB、50,000 reads/day、20,000 writes/day、20,000 deletes/day
が無料で、named database は無料枠の対象外である。アプリは事故余白 20% を残し、毎日
40,000 reads、16,000 writes、16,000 deletes を hard reservation として日次 aggregate と
32 個の監査用 accounting bucket に記録する（bucket は同一 document 内の内訳であり、
Firestore contention shard ではない）。read transaction は最大 bounded query 1001 reads
＋quota document read/write を、mutation は最大 32 reads / 16 writes を mutation 前に
予約し、超過は fail closed する。read の 6,000 は bounded query の fan-out を含む保守的な
境界値である。

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

```sh
cp infra/terraform.tfvars.example /tmp/agentic-loop.tfvars
# project_id、既存 repository、64桁 image_digest、IAP owner を /tmp で編集
TFVARS_FILE=/tmp/agentic-loop.tfvars devbox run --pure -- ./scripts/infra-plan.sh
```

plan の内容を人間が確認してから、明示的な承認を得た環境でのみ apply する。

```sh
cd infra
tofu init -input=false
tofu apply -input=false -var-file=/tmp/agentic-loop.tfvars
```

この環境では `gcloud auth list` と project を確認できない場合があるため、ローカルでの
cloud apply は行わない。`tofu plan -refresh-only` は drift 検知であり変更を適用しない。

## drift、rollback、destroy

```sh
TFVARS_FILE=/tmp/agentic-loop.tfvars devbox run --pure -- ./scripts/infra-drift.sh
```

image digest の rollback は、既知の正常 digest を `image_digest` に戻して plan/apply する。
Cloud Run と Firestore は `prevent_destroy`/`deletion_protection` で保護され、destroy は
通常の経路では拒否される。緊急削除は別途明示承認、state backup、影響範囲の記録を先に
完了し、`tofu state rm` で保護を迂回しない。
