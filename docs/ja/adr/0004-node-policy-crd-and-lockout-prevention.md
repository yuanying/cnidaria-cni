# ADR 0004: NodePolicy は cluster-scoped の CRD とし、ノードを締め出せないルールを持つ

- 状態: 決定（2026-09-19）

## 背景

NetworkPolicy が統べるのは Pod だけである。Kubernetes API の何も、ノード自身のソケット
—— kubelet、SSH、control-plane ノードの API サーバー —— に何が届いてよいかを記述しない
ので、cnidaria はそのための CRD を足し、ADR 0003 の `input` と `output` チェインへ
レンダリングする。

これはプロジェクトの中で最も障害を起こしうる部分である。control-plane ノードに適用される
ポリシーがポート 6443 を忘れればクラスターは自分自身から切り離される。10250 を忘れれば
すべてのノードが `NotReady` になる。22 を忘れればコンソールへ足を運ぶことになる。既定で
drop するルールの最初の適用は、稼働中のクラスター上で、選択されたすべてのノードに同時に
起きる。

決めるべきことは 3 つある。オブジェクトの形、ポリシーが取り除けないルール、そして適用が
間違っていたと分かったときに何が起きるか、である。

## 決定

### 形

`NodePolicy`、cluster-scoped、API グループ `cnidaria.unstable.cloud`、バージョン
`v1alpha1`。グループは regied と同じくプロジェクトが管理するドメインの下に置き、バイナリ
名を変えても API が動かないよう、バイナリの名前はそこに含めない。

| フィールド | 意味 |
|---|---|
| `spec.nodeSelector` | Node に対するラベルセレクタ。空はすべてのノードを選ぶ |
| `spec.policyTypes` | `Ingress`、`Egress`、または両方。既定は NetworkPolicy と同じ規則で、`Ingress` は常に、`Egress` は egress ルールがあるときに含まれる |
| `spec.ingress[]` | 各エントリは `from[]` の相手と `ports[]`。type `Ingress` のポリシーに選択されたノードは、いずれかのエントリが許すものだけを `input` で受け入れる |
| `spec.egress[]` | 各エントリは `to[]` の相手と `ports[]`。同様に `output` で |
| 相手 | `cidr` と `except` を持つ `ipBlock`。Pod や namespace のセレクタはここでは相手にならない。ノードはクラスターではなくネットワークによって指される |
| ポート | NetworkPolicy と同じく `protocol`、`port`、`endPort` |
| `status.nodes[]` | 選択されたノードごとに、`observedGeneration` と、その generation が `Applied`、`RolledBack`（メッセージ付き）、`Pending` のいずれか |

語彙は意図して NetworkPolicy のものである。片方を書けるオペレーターはもう片方も書け、
同じレンダラーのパターン（相手の set、ポートのルール、「いずれかのポリシーが許可」）が
当てはまる。NetworkPolicy と同じく、ある type のポリシーに選択されていないノードはその方向
について開いており、1 つでも選択されたノードは列挙されたもの以外について閉じる。1 つの
ノードを選ぶ複数のポリシーは和集合になる。

安全ルールが必要とするポートと API サーバーのアドレスはデーモンへの引数であり、既定値は
クラスターの慣習に合わせる（NodePort レンジ `30000-32767`、API サーバー `6443`、kubelet
`10250`、etcd `2379-2380`、SSH `22`）。

### ポリシーが取り除けない安全ルール

すべての `input` と `output` チェインは、NodePolicy が存在するかどうかにかかわらず、
ポリシーが参照される前に以下のルールで始まる。これらはポリシーではなく、それを切る
フィールドは無い。

| 方向 | ルール | 理由 |
|---|---|---|
| 両方 | `ct state established,related accept` | ノードが開いたものへの返事。接続に紐づく ICMP エラー |
| 両方 | loopback インターフェースを accept | 自分自身と話せないホストは、どのポリシーも意図しない形で壊れている |
| 両方 | ICMP: echo-request、destination-unreachable、time-exceeded、parameter-problem。ICMPv6: 同じものに加えて packet-too-big、router / neighbour の solicitation と advertisement、MLD の listener 各種 | Path MTU discovery、近隣探索、アドレス解決。ND が無ければ IPv6 ノードは自分のセグメントから消える |
| input | TCP `22` | コンソール無しでの復旧 |
| input | TCP `10250` | kubelet。無ければノードは `NotReady` になり `exec` / `logs` が失敗する |
| input | TCP `6443` | control-plane ノードの API サーバー |
| input | TCP `2379-2380` | control-plane ノードの etcd クライアント / ピアポート |
| input | TCP と UDP の NodePort レンジ | ノードで公開される Service。大半のノードでは DNAT がこれを `forward` へ移すが、hostNetwork のエンドポイントには `input` を通って届く |
| output | API サーバーへの TCP `6443` | デーモン自身の接続、kubelet の接続、kube-proxy の接続 |
| output | TCP `2379-2380` | etcd のピアと API サーバーのクライアント接続 |
| output | TCP `10250` | API サーバーから他ノードの kubelet への到達 |
| output | TCP と UDP の `53` | 名前解決 |
| output | このノード自身の Pod CIDR | kubelet の probe とローカル Pod への `exec` |

L2 モードの MetalLB はこの表に無いが、それは必要が無いからである。ARP は IP ではなく
`inet` テーブルに届くことは決してなく、IPv6 の近隣探索は ICMPv6 のルールでカバーされ、
ロードバランサーアドレス宛のトラフィックは kube-proxy によって DNAT され `forward` を
通る。speaker の memberlist ポートはオペレーターが普通に許可するものであり、忘れた場合に
それを捕まえるのが下の仕組みである。

これらを無条件に accept するのは、最初のバージョンにおける意図的な粗さである。
オペレーターは NodePolicy で SSH を管理用レンジに絞ることができない。安全ルールを送信元で
絞るのはこの記録への後の変更であって、安全ルール無しで始める理由にはならない。

### commit-confirmed な適用

ノードのルールを変えるすべての適用は、新しいルールセットを通して API サーバーに到達できる
まで暫定である。

1. デーモンは最後に確認済みのテーブルのテキストを保持する。
2. 新しいテーブルを適用し、タイマー（既定 30 秒）を開始する。
3. 新しい接続を張って、自分自身の Node オブジェクトを要求する。接続を意図して新しくする
   のは、`ct state established` のルールが壊れたポリシーを正常に見せることのないように
   するためである。
4. タイマーが切れる前に成功すれば、新しいテキストが最後の確認済みになり、このノードの
   status は `Applied` になる。
5. そうでなければ、最後の確認済みテキストを再適用し、API サーバーに再び到達できたときに
   ポリシーの status へ理由とともに `RolledBack` を記録し、そのポリシーのその generation は
   再試行しない。新しい generation（編集）はやり直しになる。

このチェックは粗い —— API サーバーへ到達できることは証明するが SSH については証明しない
—— ので、安全ルールも併せて存在する。2 つは別の間違いを防ぐ。安全ルールはノードが決して
失ってはならないポートをカバーし、確認はオペレーター自身の egress ルールがデーモンの
修正を受け取る能力を壊す場合をカバーする。

起動時、デーモンは API を一度 list するまでノードポリシーについて何も適用しない。以前の
実行による `inet cnidaria` テーブルが存在し、確認の時間枠内に API サーバーへ到達できない
場合、デーモンはそのテーブルの `input` と `output` チェインを安全ルールだけを持つように
書き換え、その後も試行を続ける。したがって、前回の実行で締め出したノードで再起動された
デーモンは、自分で扉を開ける。

### 手書きではなく生成する

Go の型は controller-gen のマーカーを持つ。`deploy/crd` の下の CRD マニフェストと DeepCopy
メソッドはそこから生成される（ADR 0007）。`kustomize build` にツールが要らないよう、
マニフェストはコミットする。

## 帰結

- Pod からノード自身のソケットへの ingress（hostNetwork のサービスに届く Pod、ノードから
  scrape されるメトリクス）は、`ipBlock` に Pod CIDR を使って、他の送信元と同じく NodePolicy
  が統べる。ノードポリシーの相手としての Pod セレクタは、具体的な必要が現れたときに
  足せる。省くことで最初のバージョンの相手は静的に保たれ、安全ルールの推論が単純になる。
- cluster-scoped オブジェクトの status は、選択されたすべてのノードが書く。status
  サブリソースとノードごとのエントリが、それらの書き込みの衝突を防ぐ。
- オペレーターが NTP やパッケージミラーを忘れたノードの type `Egress` のポリシーは、
  それらを黙って壊す。これは既定拒否の通常の帰結であり、`Egress` type がオプトインで
  あるのはそのためである。
- netns テストベッド（ADR 0008）は両方の半分を assert する。選択されたノードが列挙されて
  いないポートを拒否すること、そして kubelet 風と API サーバー風の接続が生き残ることで
  ある。
