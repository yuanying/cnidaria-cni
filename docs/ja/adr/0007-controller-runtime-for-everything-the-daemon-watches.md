# ADR 0007: デーモンは API を controller-runtime で watch し、kubebuilder の雛形は使わない

- 状態: 決定（2026-09-19）

## 背景

デーモンは 5 種類のオブジェクトに反応する。Node（経路、マスカレードのセット、conflist）、
Pod と Namespace（セレクターがどのアドレスを指すか）、NetworkPolicy、そして自前の
NodeNetworkPolicy CRD である。これらすべてがノードごとに 1 つの出力 —— ルーティングテーブルと
1 つの nftables テーブル —— に流れ込むので、欲しいのはそれらのオブジェクトのキャッシュと
「何かが変わった」という合図であって、オブジェクトごとの帳簿ではない。

それを得る 3 つの方法を比較した。

| 選択肢 | core 型 | CRD | 何がかかるか |
|---|---|---|---|
| どこでも client-go の informer | `SharedInformerFactory` | 手書きの REST クライアントと scheme 登録、または code-generator の clientset / lister / informer | code-generator を使わないかぎり仕組みが 2 つになる。code-generator は生成パイプラインと数千行の生成コードを加える |
| controller-runtime | Manager のキャッシュ | 同じキャッシュ。型を scheme に登録する | 依存の木が 1 つ。何を読むにも 1 つのやり方 |
| kubebuilder の雛形経由の controller-runtime | 同上 | 同上 | `PROJECT` ファイル、規定のツリー（`api/`、`controllers/`、`config/`）、webhook 付き operator 製品を前提とした Makefile と Dockerfile の規約 |

このプロジェクトの恒常的な指示は、コードとアーキテクチャが人間に読みやすいこと、そして
間接層はいま必要なときにだけ足すこと、である。

## 決定

**controller-runtime をライブラリとして使い、core 型も CRD もすべての watch をこれで
行う。kubebuilder の雛形は一切使わない。**

- 既定のキャッシュを持つ `Manager` を 1 つ。キャッシュの下は client-go の informer なので、
  選択肢 1 に対して失うものは無く、すべてのオブジェクトは manager のクライアントを通して
  同じやり方で読む。
- CRD の Go 型は `internal/apis/v1alpha1` に置き、controller-gen のマーカーを付ける。
  `make generate` が DeepCopy メソッドと CRD マニフェストを生成する。controller-gen は
  `go.mod` の `tool` ディレクティブに書いたツールで、`go tool` を通して実行する。グローバル
  には何も導入しない。code-generator は使わない。
- reconciler は 2 つ。どちらも小さく、どちらも差分を追跡するのではなくキャッシュから
  計算し直す。

  | Reconciler | トリガー | すること |
  |---|---|---|
  | routes | Node の追加 / 更新 / 削除 | 経路の全集合を計算し直す（ADR 0006）。自ノードのオブジェクトに対しては conflist を 1 度書く（ADR 0001） |
  | ruleset | Pod、Namespace、NetworkPolicy、NodeNetworkPolicy、Node | `inet cnidaria` をレンダリングして適用する（ADR 0003、0004） |

  ruleset reconciler へのイベントはすべて 1 つの固定のリクエストキーに写像するので、
  ワークキューが Pod イベントのバーストを 1 回の reconcile にまとめる。これが ADR 0003 が
  求めるデバウンスであり、キューの通常の振る舞いであって、独自の仕組みではない。
- リーダー選出は無し（各ノードは自分のためだけに動く）、webhook は無し、metrics や health の
  エンドポイントは DaemonSet の probe が必要とする分だけを localhost に束縛する。
- 小さな control-plane ノードでのメモリは、キャッシュされるすべてのオブジェクトから
  `managedFields` などの嵩張る部分を落とすキャッシュ変換と、レンダラーが読むフィールドだけ
  で Pod を watch することで抑える。Pod のキャッシュは必然的にクラスター全体である。
  このノード上の NetworkPolicy の peer は、他のどのノードの Pod も指しうる。
- パッケージ構成はこのリポジトリ独自のものである。`internal/controller` に reconciler を
  置き、ドメインのパッケージ（`routes`、`netpol`、`nodepol`、`nftables`、`conflist`）は
  controller-runtime を知らず、クラスター無しでテストできる。

## 帰結

- controller-runtime を知っている読者は見慣れた `Reconcile` の形を見つける。知らない読者は
  2 つの関数と 1 つの manager を見つけ、その周りに生成されたフレームワークは無い。
- controller-runtime のバージョンが `k8s.io` モジュールのバージョンを固定する。使用中の
  クラスターバージョンと互換になるように選び、まとめて動かす。
- NodeNetworkPolicy の status 更新（ADR 0004）は、manager のクライアントで status サブリソースを
  通して行う。すべての読み取りと同じクライアントである。
- 後にオブジェクトごとの reconcile が必要になったら —— たとえば独自の再試行を要する
  NodeNetworkPolicy ごとの condition —— それは 3 つ目の小さな reconciler であって、ここにある 2 つの
  変更ではない。
