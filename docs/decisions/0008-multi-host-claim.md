# ADR 0008: 複数ホスト間のIssue claimをGitHub上で調停する

## 状態

採用

## 背景

`agent:queued`から`agent:running`へのLabel置換はcompare-and-swapではない。複数ホストのSupervisorが同じqueued snapshotを読むと、双方がLabel変更後の`running`だけを確認して同じIssueのworkerを起動できる。PID、lock、scope cacheはGit common stateにあり、別ホスト間の排他には使えない。

## 決定

Label変更より前に、各候補は期限付き`agentic-loop:claim`コメントを作成する。同じIssueについて有効なclaimコメントのうち、GitHubが割り当てた最小comment idを持つ候補だけを所有者とする。敗者は自分のclaimを期限切れへ更新し、Label、Git、workerを変更しない。

勝者のclaimコメントは`agentic-loop:lease`も兼ね、以降のheartbeatは同じコメントをPATCHする。worker停止後にコメントが残っても、期限後は次のホストが回収できる。Labelは人向け状態、claim/leaseコメントはホスト間所有権の正本とする。

Supervisorごとの`max_workers`はローカル上限である。複数ホストを起動するとrepository全体の同時worker数は各ホスト上限の合計まで増え得るため、費用・GitHub API・端末資源を踏まえて各ホストの設定を下げる。

systemd unitは固定環境の`devbox run`を入口にする。生成時点の一時的なNix profileにある`yq` pathをunitへ固定せず、再起動後も設定を読めるようにする。

## 結果

- 同じIssueを複数ホストが同時に実行しない。
- 他ホストの有効なleaseは既存の回復処理が尊重する。
- GitHub comment作成・取得ができない場合はclaimを成立させず、安全側でそのpollを見送る。
- claimごとにREST callと短い調停待ちが増える。
