# ADR 0004: NodePolicy は cluster-scoped の CRD とし、既定を permissive にして、ノードを締め出せないルールを持つ

- 状態: 決定（2026-09-19）。同日改訂: この記録の最初の版が選んだ commit-confirmed な
  適用を、permissive モードに置き換えた。

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

決めるべきことは 3 つある。オブジェクトの形、ポリシーが取り除けないルール、そして
ポリシーが何を壊すかを、壊す前にオペレーターがどうやって知るか、である。

## 決定

### 形

`NodePolicy`、cluster-scoped、API グループ `cnidaria.unstable.cloud`、バージョン
`v1alpha1`。グループは regied と同じくプロジェクトが管理するドメインの下に置き、バイナリ
名を変えても API が動かないよう、バイナリの名前はそこに含めない。

| フィールド | 意味 |
|---|---|
| `spec.mode` | `Permissive`（既定）または `Enforce`。`Permissive` ではポリシーを完全にレンダリングするが何も drop しない。drop されるはずだったものは代わりにログに出し、数える。SELinux と同じく、enforce は明示的に選ぶ |
| `spec.nodeSelector` | Node に対するラベルセレクタ。空はすべてのノードを選ぶ |
| `spec.policyTypes` | `Ingress`、`Egress`、または両方。既定は NetworkPolicy と同じ規則で、`Ingress` は常に、`Egress` は egress ルールがあるときに含まれる |
| `spec.ingress[]` | 各エントリは `from[]` の相手と `ports[]`。type `Ingress` のポリシーに選択されたノードは、いずれかのエントリが許すものだけを `input` で受け入れる |
| `spec.egress[]` | 各エントリは `to[]` の相手と `ports[]`。同様に `output` で |
| 相手 | `cidr` と `except` を持つ `ipBlock`。Pod や namespace のセレクタはここでは相手にならない。ノードはクラスターではなくネットワークによって指される |
| ポート | NetworkPolicy と同じく `protocol`、`port`、`endPort`。ただしポートは常に数値で指定する。ノードにはコンテナポートが無く、名前が指すものが存在しないため |
| `status.nodes[]` | 選択されたノードごとに、`observedGeneration`、その generation を適用した `mode`、そしてレンダリングできなかったときの `message` |

語彙は意図して NetworkPolicy のものである。片方を書けるオペレーターはもう片方も書け、
同じレンダラーのパターン（相手の set、ポートのルール、「いずれかのポリシーが許可」）が
当てはまる。NetworkPolicy と同じく、ある type のポリシーに選択されていないノードはその方向
について開いており、1 つでも選択されたノードは列挙されたもの以外について閉じる。1 つの
ノードを選ぶ複数のポリシーは和集合になる。そしてノードがある方向について enforce になる
のは、その方向でそのノードを選ぶすべてのポリシーが `Enforce` のときだけである。permissive
のポリシーが 1 つでもあれば、ノードは観察のままである。

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
それを見せるのが permissive モードである。

これらを無条件に accept するのは、最初のバージョンにおける意図的な粗さである。
オペレーターは NodePolicy で SSH を管理用レンジに絞ることができない。安全ルールを送信元で
絞るのはこの記録への後の変更であって、安全ルール無しで始める理由にはならない。

### 既定は permissive

NodePolicy はどちらのモードでも同じようにレンダリングされる。安全ルール、次にポリシーの
チェインへの dispatch、そしてどのルールにも accept されなかったパケットへの verdict。
違うのはその最後の verdict だけである。

| モード | dispatch チェイン末尾の verdict |
|---|---|
| `Permissive` | `limit rate` → `log prefix "cnidaria-nodepolicy "` → `counter` → base chain の `policy accept` に落ちる |
| `Enforce` | `counter` → `drop` |

`Permissive` では、ポリシーが拒否するはずのパケットは accept され、その送信元・宛先・
プロトコル・ポートが上の prefix つきで kernel log（したがってノードの journald）に出て、
ルールのカウンタが総数を保つ。rate limit は忙しいノードがログを溢れさせないためにあり、
カウンタは rate limit の対象外なので、ログが間引かれても数は正確である。

ポリシーを本番に入れる手順はしたがって次のようになる。

1. `Permissive` で適用する。`spec.mode` の無いオブジェクトはこれになる。
2. トラフィックのパターンが必要とするだけの間、ログとカウンタを見る —— 夜間にバックアップが
   走るノードなら丸一日。
3. ログが欠けていると示したエントリを足し、`spec.mode: Enforce` にする。

2 つの層は別の間違いを防ぐ。**安全ルール**は、ポリシーが何を言おうとノードが決して失って
はならないもののためにある。両方のモードで効き、オペレーターの操作を要しない。
**permissive モード**は、オペレーターが忘れたそれ以外のすべて —— メトリクスの scraper、
memberlist のポート、バックアップ先 —— のためにある。前もって列挙できず、失ってもノードは
生き残るが、不意に失うべきではないものである。前者はポリシーがノードを締め出せないように
し、後者はポリシーの効果の全体を、効果が出る前に見えるようにする。

モードの間でルールセットに変わるものは verdict 1 つだけなので、`Permissive` で観察した
ものは、`Enforce` がすることそのものである。信じるべき 2 つ目のレンダリングは無い。

### 検討して採らなかったもの: commit-confirmed な適用

この記録の最初の版は commit-confirmed な適用を選んだ。ノードのルールへのすべての変更は
暫定で、デーモンは新しいルールセットを通して API サーバーへ新しい接続を張り、タイマー内に
到達できなければ前のルールセットに戻して、その generation を失敗と記録する。起動時には、
残っているテーブルと到達できない API サーバーの組み合わせを、安全ルールだけに書き戻す。

置き換えたのは、証明できることに対して掛かるものが大きいからである。仕組みは、タイマー、
専用の probe 接続、記憶された最後の正常なルールセット、何が失敗したかの generation ごとの
記憶、そして前回の実行を取り消す起動時の経路 —— 状態機械であり、その機械自身のバグは
障害の最中に走るコード経路に座ることになる。そしてそのすべてが証明するのは 1 つのこと、
API サーバーへ到達できることだけである。SSH についても、メトリクスの scraper についても、
バックアップ先についても何も言わない。permissive モードには状態機械が無く —— verdict を
1 つ入れ替えるだけ —— ポリシーの効果の全体を見せる。probe が確認できる 1 つの効果ではなく。

安全ルールと permissive の段階があってもなお `Enforce` の適用が実際に締め出しを起こしたら、
その決定を再検討する場所はこの記録である。

### 手書きではなく生成する

Go の型は controller-gen のマーカーを持つ。`deploy/crd` の下の CRD マニフェストと DeepCopy
メソッドはそこから生成される（ADR 0007）。`kustomize build` にツールが要らないよう、
マニフェストはコミットする。

## 帰結

- デーモンはノードポリシーについて、適用をまたぐ記憶を必要としない。各 reconcile は見えて
  いるオブジェクトから、それぞれが宣言するモードでルールセットをレンダリングして適用する
  （ADR 0003）。起動も他の reconcile と同じである。
- Pod からノード自身のソケットへの ingress（hostNetwork のサービスに届く Pod、ノードから
  scrape されるメトリクス）は、`ipBlock` に Pod CIDR を使って、他の送信元と同じく NodePolicy
  が統べる。ノードポリシーの相手としての Pod セレクタは、具体的な必要が現れたときに
  足せる。省くことで最初のバージョンの相手は静的に保たれ、安全ルールの推論が単純になる。
- cluster-scoped オブジェクトの status は、選択されたすべてのノードが書く。status
  サブリソースとノードごとのエントリが、それらの書き込みの衝突を防ぐ。
- `Permissive` のままのポリシーは何も守らない。それは `status.nodes[]` とオブジェクト自身に
  見えており、意図した既定である。enforce はオペレーターが選ばなければならない。
- ログの prefix はインターフェースの一部である。journal からそれを読むツールは、prefix が
  変わらないこと、フィールドが nftables の `log` が出すものであることを前提にしてよい。
- NetworkPolicy に同じ `mode` を付けること —— Pod のポリシーが drop するものを drop する前に
  観察する —— は自然な後の追加であり、本タスクの範囲外である。
- netns テストベッド（ADR 0008）は、選択されたノードについて、列挙されていないポートが
  `Permissive` ではログに出て数えられつつ届くこと、`Enforce` では拒否されること、そして
  kubelet 風と API サーバー風の接続がどちらでも生き残ることを assert する。
