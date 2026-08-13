# ADR 0010: 認可済みのIssue終了・統合

## 決定

要求撤回は `agent:cancelled`、後続要求への置換は `agent:superseded`、同一成果の重複は `agent:duplicate`、別要求の統合は `agent:merged` とする。これらは `completed`、`failed`、`stale` と混用しない終端状態であり、`agent:*` Labelが正本である。実行中の終了は一度 `agent:stopping` に入り、ローカル所有workerだけをTERM、設定された猶予後にKILLしてから終端化する。

公開入口は `bin/agentic-loop dispose ISSUE --reason ... [--target ISSUE]` と `resume ISSUE` に限定する。GitHub認証済み実行者が対象repositoryの `write`、`maintain`、`admin` 権限を持つことをRESTで確認する。workerの自己申告とbot commentは認可根拠にしない。統合理由では同一repositoryのopenかつ未終了の統合先を必須とし、自己参照を拒否する。

終了・統合では実行者、理由、対象、時刻をmachine-readable markerと日本語コメントに残す。統合先には元Issueの本文・全コメント・依存関係も要求として確認するmarkerを残し、本文や秘密を複製しない。終了時は `state_reason=not_planned` でcloseする。resumeは認可済みの終端Issueだけを履歴を残してopen + `agent:queued` に戻す。

## 安全性

local workerの停止はprocess group単位だが、dirty worktree、未push commit、local branch、remote branchは削除しない。終了済みPRも自動削除しない。merge済みPRを持つ `agent:completed` Issue の取消は拒否し、revertまたは後続Issueを要求する。別hostのleaseは破壊的に扱わず、所有hostによるdrainを待つ。

## 帰結

Supervisorのclaim、failed retry、needs-input回答、依存回復、lease回復は `agent:queued` だけを入力とするため、終端Issueを自動再queueしない。Project、status、metricsは通常完了・失敗・staleと終了理由を別に表示する。追加の外部serviceや課金は発生しない。
