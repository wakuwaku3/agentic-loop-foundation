# Provider registry

Provider registry は Codex、Claude、OpenCode の観測済み状態を Control Plane から
読み取る surface であり、CLI を probe しない。未観測は `unknown` かつ `stale` とし、
沈黙を healthy と解釈しない。authorization は直接 runner session の選択を表し、
repository 内の承認 record を参照しない。

runaway detector の具体的なしきい値と counter は runner-local の invocation policy
および外部 ledger が正本である。registry は `within-thresholds`、
`stopped-for-inspection`、`unknown` の状態だけを公開し、金額や credential を返さない。

Version に関する authority は次の通り。

| fact | authority |
|---|---|
| which Provider CLI versions an adapter supports | the source-declared interval in `internal/provider` |
| which Loop versions carry that adapter contract | the Loop's own release identity read through the existing `ReleaseObserver` |
| what version a CLI actually is on a machine | the runner session invocation policy |
| whether the declared support statement is true of a real CLI | nothing in this repository; only a direct live exercise |

実 CLI の version、session ID、usage は runner 側で検証・集計し、Control Plane の
Provider response envelope へ raw 値を転送しない。互換性の最終確認は fixture ではなく
Codex/OpenCode を直接起動する live test で行う。
