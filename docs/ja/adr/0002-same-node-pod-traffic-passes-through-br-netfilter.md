# ADR 0002: 同一ノード内の Pod 間トラフィックは br_netfilter を通してフィルタする。クラスターは既にそれを要求している

- 状態: 決定（2026-09-19）

## 背景

同じノード上の 2 つの Pod は、同じ bridge、同じサブネットにいる。両者の間のフレームは
bridge が L2 でスイッチし、既定ではホストの IP スタックに一切入らない。`prerouting` も
`forward` も `postrouting` も通らない。IP 層のフックにレンダリングされた NetworkPolicy は、
ノード間のトラフィックには効き、1 つの bridge 上の隣人同士のトラフィックには盲目になる。
動いているように見えるポリシーという、最悪の種類のポリシーである。

カーネルの答えは `br_netfilter` モジュールと、sysctl の
`net.bridge.bridge-nf-call-iptables` および `net.bridge.bridge-nf-call-ip6tables` を `1` に
することである。これにより、bridge された IPv4 / IPv6 フレームはルーティングされたかの
ように IP 層の netfilter フックを通り、`forward` がそれを見て conntrack が追跡する。

これはノードへの新しい要求ではない。iptables モードの kube-proxy が同じ理由で同じものを
必要としている。Service へ話しかける Pod は `prerouting` で別の Pod へ DNAT され、その Pod
は同じ bridge 上にいるかもしれず、そこからの返事は DNAT を戻さなければならない。返事が
netfilter に入らず L2 でスイッチされると、クライアントは送った覚えのないアドレスからの
返事を見て、接続は失敗する。このため:

- クラスターのバージョン（v1.29）の Kubernetes ドキュメントは、コンテナランタイムの前提
  条件のページの「Forwarding IPv4 and letting iptables see bridged traffic」の下に、
  `br_netfilter` のロードと合わせてこの 2 つの sysctl を挙げている。
- kube-proxy の iptables proxier は起動時に `bridge-nf-call-iptables` を読み、`1` でなければ
  proxy が「意図どおりに動かないかもしれない」とログに出す。
- 同じバージョンの kubeadm の preflight チェックは
  `/proc/sys/net/bridge/bridge-nf-call-iptables`（IPv6 使用時は `ip6tables` の対も）を読み、
  `1` を期待する。

対象となるすべてのノードは kube-proxy を満たすように構築されているので、sysctl は既に
設定されている。

br_netfilter 無しで同一ノードのトラフィックを見られる場所が他に 2 つあり、検討した。

- **nftables の bridge ファミリ**（`table bridge`）は bridge 自身の経路にフックする。
  すべてのフレームを見るが、そこでは conntrack が使えないため「established」を表現
  できず、すべての返事に専用のルールが要る。また、すべてのポリシーの写しを 2 つ目の
  テーブルに持つことにもなる。
- **bridge 無しの Pod ごとの veth**（各 Pod に /32 と /128 を与え、ホストがルーティング
  する）は、すべてのパケットをルーティングされたパケットにする。これは本プロジェクトが
  意図して保つ flannel `host-gw` のデータプレーン（ADR 0001）とは別のデータプレーンである。

## 決定

**cnidaria は、両方の sysctl が 1 に設定された br_netfilter を前提とし、そうでなければ
起動を拒否する。**

- デーモンは起動時に `net.bridge.bridge-nf-call-iptables` と
  `net.bridge.bridge-nf-call-ip6tables` を読む。`1` 以外の値、あるいはモジュールが
  ロードされておらずファイルが存在しないことは、その sysctl 名と求める値を示す
  メッセージ付きの致命的エラーである。
- 同じチェックが `net.ipv4.ip_forward` と `net.ipv6.conf.all.forwarding` も見る。Pod の
  ためにルーティングするノードには、いずれにせよ必要なものである。
- デーモン自身はモジュールをロードせず、sysctl も設定しない。それはノードのプロビジョ
  ニングの仕事であり、クラスターは既に kube-proxy のためにそこで設定している。カーネル
  全体の設定を黙って変えるデーモンは、必要なものを言うデーモンより理解しにくい。

警告ではなく起動を拒否するのは意図的である。他のログ行の流れに埋もれた警告こそが、
ポリシーが黙ってトラフィックの半分に効いていない状態を生む。起動しないデーモンは
気づかれるし、どのポリシーも信頼される前に気づかれる。

br_netfilter があれば、2 つの Pod の間のすべてのパケットは —— 同じノードでも別のノード
でも —— 通過するノードごとにちょうど 1 回ホストの `forward` フックを通り、conntrack が
両方向を見る。ADR 0003 のチェイン配置はこれに依拠する。

## 帰結

- netns テストベッド（ADR 0008）は「ノード」の namespace に同じ sysctl を設定しなければ
  ならない。テストハーネスはこれを明示的に行い、前提がテストのある場所に書かれるように
  する。
- マッチはアドレスで行い、bridge ポート名では行わない。br_netfilter の下では、`forward`
  で見える bridge されたフレームは入力インターフェースも出力インターフェースも bridge
  そのものを報告するので、インターフェースのマッチでは 2 つの Pod を区別できない。
  ADR 0003 の Pod set はアドレスの set である。
- 将来の kube-proxy が br_netfilter を必要としなくなっても、cnidaria は依然として必要と
  する。起動時チェックはそのために cnidaria 自身のものであり、kube-proxy が報告する何かに
  委ねない。
