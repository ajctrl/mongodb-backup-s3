# mongodb-backup-s3

MongoDB のデータ・認証情報・構成を S3 / S3 互換ストレージへ保存する Go 製ツールです。
`mariadb-backup-s3-main` の S3 転送、任意の GPG 暗号化、スケジュール、保存期間、
固定名と S3 バージョン指定復元を引き継いでいます。参照元のディレクトリは変更しません。
元のライセンスと帰属は `LICENSE` / `LICENSE.txt` に保存しています。

| `BACKUP_MODE` | 保存単位 | バックアップ中の書き込み |
| --- | --- | --- |
| `full`（既定） | レプリカセット全体を 1 オブジェクト | `--oplog` を付け、復元時に `--oplogReplay` で変更履歴を適用 |
| `per-database` | 選択した DB ごとに 1 オブジェクト | oplog なし。同じ DB・コレクション内でも同一時点の整合性は保証しない |

対象は自前運用のレプリカセットです。シャーディング構成、スタンドアロン、
継続的な任意時点復元は対象にしません。`full` では DB・コレクションの絞り込みや除外を受け付けません。
MongoDB の `--oplog` は全体ダンプ専用です。
[MongoDB のバックアップ仕様](https://www.mongodb.com/docs/database-tools/mongodump/)

## ビルドと実行

ビルドには Ubuntu 24.04、Go 1.26、MongoDB Database Tools **100.18.0** を使用します。
Linux amd64 / arm64 に対応し、公式配布の `mongodump` と `mongorestore` を収録します。
最終イメージは `scratch` をベースに、Ubuntu の実行用ライブラリ、GPG、`/bin/sh`、
CA 証明書、タイムゾーンとライセンスを収録します。apt や一般的な管理コマンドは含みません。
amd64 での展開後レイヤー合計は約 **89 MB**（従来の約164 MBから約46%削減）。
Docker の `DISK USAGE` は圧縮データも含み、この環境では約127 MBです。
サイズはビルド時のパッケージ更新やアーキテクチャによって変わります。
Database Tools と MongoDB Server のバージョン体系は別です。結合テストは MongoDB 8.0 系を対象とします。
運用中と同じ Server / FCV / Tools の組み合わせで復元を確認してください。
[公式配布一覧](https://www.mongodb.com/try/download/database-tools/releases/archive)

```sh
cp template.env .env
# .env の MongoDB 接続情報と既存の S3 バケットを設定する
docker build -t mongodb-backup-s3:local .
docker run --rm --env-file .env mongodb-backup-s3:local
```

既定の `run` は `SCHEDULE` が空なら 1 回実行して終了します。
`SCHEDULE=CRON_TZ=Asia/Tokyo 0 3 * * *` なら次の実行時刻まで待機し、毎日 03:00 に実行します。
スケジュールの設定に関係なく即時実行する場合は `backup` を使います。

```sh
docker run --rm --env-file .env mongodb-backup-s3:local mongodb-backup-s3 backup
```

同じプロセスでのスケジュール実行は重複しません。同じ保存先に書き込むコンテナを複数起動しないでください。
処理失敗はログに記録され、単発実行では非ゼロで終了します。

## MongoDB 接続と権限

URI には DB パスを指定せず、認証先は `authSource` に設定します。
ユーザー名・パスワード内の予約文字は URL エンコードしてください。

```dotenv
MONGODB_URI=mongodb://backup:CHANGE_ME@mongo1:27017,mongo2:27017,mongo3:27017/?replicaSet=rs0&authSource=admin
MONGODB_READ_PREFERENCE=primary
```

URI を秘密ファイルで渡す場合は `MONGODB_URI` を空にして、
`MONGODB_URI_FILE=/run/secrets/mongodb_uri` とファイルの読み取り専用マウントを指定します。
両方の指定はエラーになります。URI は子プロセスの引数に置かず、権限 `0600` の一時設定ファイル経由で渡します。

TLS は URI の `tls=true` で有効にします。独自 CA を使う場合は CA ファイルをマウントし、
`MONGODB_TLS_CA_FILE` にコンテナ内のパスを指定します。サーバー設定として収集した証明書は、
接続設定として自動適用されません。

バックアップ用アカウントには `admin` の `backup` と `clusterMonitor` ロールを付与します。
`clusterMonitor` は実際のレプリカセット構成・状態の取得に使用します。
運用管理者が `mongosh` で実行する設定例です。

```javascript
db.getSiblingDB('admin').createUser({
  user: 'backup',
  pwd: 'CHANGE_ME',
  roles: [
    { role: 'backup', db: 'admin' },
    { role: 'clusterMonitor', db: 'admin' }
  ]
});
```

`MONGODB_READ_PREFERENCE=secondary` はバックアップの読み取りをセカンダリに限定します。
`secondaryPreferred` はセカンダリが選べない場合にプライマリも利用します。
読み取り先での I/O 負荷、レプリケーション遅延、oplog の保持時間を確認してください。
復元は書き込み先へ接続し、このバックアップ用設定でセカンダリへ復元することはありません。

## DB 単位の保存

```dotenv
BACKUP_MODE=per-database
MONGODB_DATABASE=
MONGODB_DATABASES=app,analytics
MONGODB_BACKUP_ALL=false
MONGODB_DATABASES_EXCLUDE=
```

単一 DB は `MONGODB_DATABASE=app`、複数 DB は `MONGODB_DATABASES=app,analytics` で指定します。
両方が設定されている場合は `MONGODB_DATABASES` を優先します。
自動検出は明示指定を空にして、次のように設定します。

```dotenv
BACKUP_MODE=per-database
MONGODB_DATABASE=
MONGODB_DATABASES=
MONGODB_BACKUP_ALL=true
MONGODB_DATABASES_EXCLUDE=scratch,temp
```

`MONGODB_BACKUP_ALL=true` と明示指定は併用できません。除外リストは自動検出専用です。
自動検出では `admin`、`config`、`local` を除き、これらの内部 DB の明示指定も拒否します。
認証情報は別の `auth.archive.gz` として各 DB のバンドルに含めます。
これは **全 DB のユーザー・ロールを含む admin DB のダンプ**で、選択した DB だけの認証情報ではありません。
DB ごとにデータと認証情報を順に取得するため、両者は同一時点のスナップショットではありません。

`per-database` の取得中に書き込みがあると、更新前後のデータが混ざる可能性があります。
整合性が必要なら対象 DB への書き込み・構造変更を止めるか、`full` を使ってください。

## 設定・鍵・証明書の保存

`rs.conf()` 相当の稼働中レプリカセット構成は接続先から取得して、毎回 `replica-set.json` に保存します。
`mongod.conf`、初期化用 `rs.conf`、keyFile、TLS 証明書・秘密鍵、Compose 定義などのローカルファイルは、
MongoDB 接続だけでは取得できません。保存対象をまとめて読み取り専用マウントします。

```text
backup-config/
├── compose.yaml
├── rs.conf
├── node1/
│   ├── mongod.conf
│   └── keyFile
├── node2/
│   └── mongod.conf
└── tls/
    ├── ca.pem
    └── server.pem
```

```sh
docker run --rm --env-file .env \
  --mount type=bind,src="$(pwd)/backup-config",dst=/backup-config,readonly \
  -e MONGODB_CONFIG_DIR=/backup-config \
  mongodb-backup-s3:local mongodb-backup-s3 backup
```

複数ホストに配置したメンバーの設定は、各ホストから事前に集めてください。
設定ディレクトリは再帰的に収集し、シンボリックリンクや特殊ファイルは受け付けません。
設定ファイルの相対パスは有効な UTF-8 である必要があり、不正なバイト列を含む名前があればバックアップを失敗させます。
収集中の設定変更は避けてください。空の `MONGODB_CONFIG_DIR` はローカルファイルの収集を省略します。
収集後に一覧・内容のハッシュ・元のディレクトリの同一性を再確認し、変更を検知したらバックアップを失敗させます。
この再確認では設定ファイルをもう一度読みます。ファイルシステムの原子的なスナップショットではありません。
ディレクトリを指定した場合に読み取れないファイルがあれば、そのバックアップは失敗します。
指定したディレクトリが空の場合も、設定を保存できなかったものとして失敗します。

ユーザー・ロール・SCRAM 認証情報はダンプに含まれます。平文パスワードを取得する機能ではありません。
外部認証基盤の資格情報や、マウントしていないファイルは含まれません。
秘密情報も保存するため、必要に応じて `PASSPHRASE` を設定し、復号に必要な値は S3 とは別に保管してください。

## S3 上の構造

データと設定を **同じ ZIP64 バンドルにまとめて 1 オブジェクトとして公開**します。
ダンプ・構成収集・暗号化に失敗した場合、未完成の世代を完成済みオブジェクトとして公開しません。
別々のデータファイルと設定ファイルの世代がずれることを防ぎます。

```text
backup/full/2026-09-13T03:00:00.zip.gpg
backup/per-database/app/2026-09-13T03:00:00.zip.gpg
backup/per-database/analytics/2026-09-13T03:00:01.zip.gpg
```

復号後のバンドルは次の構造です。`data.archive.gz` は MongoDB 独自の archive 形式です。

```text
data.archive.gz       # full は全体＋oplog、per-database は対象 DB
auth.archive.gz       # per-database のみ。admin DB と全ユーザー・ロール
replica-set.json      # 稼働中のレプリカセット構成
config/...            # 任意でマウントした構成ファイル
manifest.json         # 形式・バージョン・取得日時・ファイルの SHA-256 など
```

暗号化なしなら `.zip`、ありなら `.zip.gpg` です。DB 名のパス部分は URL エンコードします。
モードのディレクトリを分け、最新検索や削除の対象が混ざらないようにしています。
`S3_PREFIX` は接続元レプリカセットごとに分けてください。
ファイル名の日時は UTC です。スケジュールで指定したタイムゾーンとは別です。

`BACKUP_FILENAME_MODE=fixed` はそれぞれ `full/latest.zip[.gpg]`、
`per-database/<DB>/latest.zip[.gpg]` を上書きします。
固定名ではアップロード前にバケットのバージョニングを確認し、無効なら停止します。
確認自体が権限不足などで失敗した場合は MariaDB 版同様、警告を出して続行するため、事前に有効化してください。

`BACKUP_KEEP_DAYS` は成功後に、対象ディレクトリにある期限切れの日時付きバックアップを削除します。
期限の判定は S3 の更新日時を使います。固定名や過去のオブジェクトバージョンは削除しません。
それらの保存期限と、失敗したマルチパートアップロードの残骸は S3 Lifecycle でも管理できます。

バックアップは archive → ZIP → 任意の GPG → S3 へストリーミングします。
ZIP64 により 4 GiB を超えるエントリも扱え、ダンプ全体をメモリや一時ディスクに置きません。
50 GB でも毎回全件を読む論理フルバックアップです。圧縮 CPU・回線速度・DB の読み取り負荷により所要時間が変わります。
`full` ではダンプ開始から完了までの oplog が必要なので、書き込み量が増えた場合も保持できる余裕が必要です。
コレクション名変更など `mongodump --oplog` が許可しない操作は取得中に避けてください。

## 復元

バックアップ時と同じ `S3_PREFIX`、`BACKUP_MODE`、`BACKUP_FILENAME_MODE`、`PASSPHRASE` を使い、
MongoDB URI を **復元先**に変更した `restore.env` を作ります。
復元先は構築済みの空の環境を基本とします。復元はロールバックされるトランザクションではなく、
失敗すると途中まで書き込まれた状態が残ります。

S3 オブジェクト全体のダウンロード・復号・ZIP / manifest / ハッシュ検証を終えてからインポートします。
検証済み ZIP 内の archive を読みながら `mongorestore` の標準入力へ渡します。
一時ディスクには、暗号化なしならバンドル 1 個分、暗号化ありなら暗号化済み・復号済みの計 2 個分の空き容量が必要です。
インポート用 archive を別ファイルとして展開するための空き容量は不要です。
`TMPDIR` や `/tmp` へのディスクマウントで容量を確保してください。
インポート後のインデックス再構築・oplog 再生も復元時間に含まれます。

```sh
# 最新のバックアップ
docker run --rm --env-file restore.env mongodb-backup-s3:local mongodb-backup-s3 restore

# 日時指定
docker run --rm --env-file restore.env mongodb-backup-s3:local \
  mongodb-backup-s3 restore 2026-09-13T03:00:00

# 固定名オブジェクトの過去バージョン
docker run --rm --env-file restore.env -e BACKUP_FILENAME_MODE=fixed \
  mongodb-backup-s3:local mongodb-backup-s3 restore --version-id VERSION_ID
```

`full` の復元では DB 選択関連の変数をすべて空／false にします。
全データとユーザー・ロールを復元し、oplog を必ず再生します。部分復元・DB 名変更はできません。
`per-database` の復元は `MONGODB_DATABASE=app` のように 1 DB を指定し、他の選択変数はクリアします。
元と同じ DB 名へデータだけを復元し、認証情報は自動変更しません。

通常の復元は `--stopOnError` を付けます。`MONGODB_RESTORE_DROP=true` は
ダンプに含まれる既存コレクションを削除してから復元します。ダンプにないコレクションは削除しません。
admin DB を `--drop` で復元するとユーザー情報も置き換わるため、復元後はバックアップ時の資格情報が必要です。
`full` の復元または `restore-auth` で `MONGODB_RESTORE_DROP=true` を指定した場合は、ユーザー・ロールを先に復元し、
新しい接続で残りの復元を行います。ユーザーの内部 ID が置き換わっても、
認証済み接続の失効で oplog 再生や admin DB のインデックス作成が止まらないようにするためです。
認証を有効にしている場合、実行アカウントは復元先とバックアップの両方に同じユーザー名・パスワードで存在し、
バックアップ内のロールにも実行する復元に必要な権限（`full` では oplog 再生を含む）が必要です。
検証済み archive は事前の認証復元と残りの復元で 2 回読みますが、追加の展開ファイルは作りません。
[MongoDB の復元仕様](https://www.mongodb.com/docs/database-tools/mongorestore/)

### 認証情報の明示的な復元

DB 単位バンドルの `auth.archive.gz` を復元する場合は、別コマンドを実行します。
これは admin DB に含まれる **全 DB のユーザー・ロールとその他の admin データ**を復元する操作です。
選択 DB のユーザーだけに限定しません。

```sh
docker run --rm --env-file restore.env \
  -e BACKUP_MODE=per-database -e MONGODB_DATABASE=app \
  mongodb-backup-s3:local mongodb-backup-s3 restore-auth
```

`restore-auth` も日時または `--version-id` を指定できます。`full` では認証情報が本体に含まれるため使いません。
復元の実行アカウントはバックアップアカウントと分けてください。
通常のデータ復元には `restore` ロールが基本ですが、oplog 再生には追加権限が必要です。
MongoDB 公式は `anyResource` に `anyAction` を持つ専用ロールを案内しています。
認証情報や `system.profile` などを含む復元についても、実際の内容に応じた権限が必要です。
[MongoDB の復元に必要な権限](https://www.mongodb.com/docs/database-tools/mongorestore/mongorestore-behavior-access-usage/)

### 設定だけの取り出し

```sh
mkdir -p recovered
docker run --rm --env-file restore.env -e MONGODB_URI= -e MONGODB_URI_FILE= \
  --mount type=bind,src="$(pwd)/recovered",dst=/recovered \
  mongodb-backup-s3:local mongodb-backup-s3 extract-config /recovered/generation
```

`extract-config OUTPUT_DIR [TIMESTAMP | --version-id VERSION_ID]` は MongoDB 接続なしで、
`replica-set.json`、`manifest.json`、`config/` を取り出します。出力先ディレクトリが既に存在すると拒否します。
設定だけを取り出す場合も、バンドル全体をダウンロード・復号し、全エントリを検証してから抽出します。
ハッシュは改変や破損の検知に使います。暗号化されていない ZIP のハッシュ自体は、送信元の真正性を証明する署名ではありません。

復旧時はファイルを取り出し、復元先のホスト名・パス・証明書に合わせて設定し、
レプリカセットを構築してからデータを復元します。設定の自動適用、`rs.initiate()`、`rs.reconfig()` は行いません。
取り出したファイルは `0600`、ディレクトリは `0700` です。元の所有者・権限は自動復元せず、
MongoDB の実行ユーザーが読めるよう配置時に調整してください。

## 環境変数一覧

| 変数 | 既定値 | 内容 |
| --- | --- | --- |
| `MONGODB_URI` / `MONGODB_URI_FILE` | 必須 | 片方を指定。`extract-config` では不要 |
| `MONGODB_READ_PREFERENCE` | `primary` | `primary` / `primaryPreferred` / `secondary` / `secondaryPreferred` / `nearest` |
| `MONGODB_TLS_CA_FILE` | 空 | TLS 検証に使う CA ファイル |
| `MONGODB_PARALLEL_COLLECTIONS` | `4` | 同時処理コレクション数。1〜128 |
| `MONGODB_CONFIG_DIR` | 空 | 保存する構成ファイルのディレクトリ |
| `MONGODB_RESTORE_DROP` | `false` | 復元前に対象コレクションを削除 |
| `BACKUP_MODE` | `full` | `full` / `per-database` |
| `MONGODB_DATABASE` | 空 | 単一 DB 指定、または DB 単位の復元対象 |
| `MONGODB_DATABASES` | 空 | バックアップする DB のカンマ区切りリスト |
| `MONGODB_BACKUP_ALL` | `false` | 業務 DB の自動検出 |
| `MONGODB_DATABASES_EXCLUDE` | 空 | 自動検出から除外する DB リスト |
| `S3_BUCKET` | 必須 | 保存先の既存バケット |
| `S3_REGION` | `us-west-1` | AWS リージョン |
| `S3_PREFIX` | `backup` | キーの接頭辞。レプリカセットごとに分ける |
| `S3_ENDPOINT` | 空 | S3 互換サービスの HTTP(S) エンドポイント。パス形式でアクセス |
| `S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY` | 空 | 任意の資格情報。AWS SDK の認証プロバイダも利用可能 |
| `S3_SESSION_TOKEN` | 空 | 一時資格情報のトークン |
| `S3_UPLOAD_PART_SIZE_MB` | `8` | マルチパートのバッファサイズ（MiB）、5〜5120 |
| `SCHEDULE` | 空 | cron 式、秒付き cron、`@daily` など |
| `BACKUP_KEEP_DAYS` | 空 | 成功後に期限切れを削除。1〜36500 日 |
| `BACKUP_FILENAME_MODE` | `timestamp` | `timestamp` / `fixed` |
| `PASSPHRASE` | 空 | 任意の GPG 対称暗号化。改行は不可 |

S3 にはアップロード・一覧取得・ダウンロードの権限が必要です。
保持期間を使う場合は削除、固定名ではバージョニング確認、バージョン指定復元では過去バージョンの取得権限も付けます。

## ローカル開発と検証

```sh
# .env の S3 設定を使う。MongoDB URI は Compose 内の開発用接続で上書きする
docker compose --env-file .env up -d --build
docker compose logs -f backup
```

Compose は外部公開ポートなしの MongoDB 8.0 単一メンバーレプリカセットを用意します。
開発専用の root / backup 資格情報、生成したノード間認証用 keyFile、`mongod.conf`、サンプルの `app` DB を使います。
設定とデータは Docker Volume に残り、バックアップにはその設定ボリュームを読み取り専用でマウントします。
本番の MongoDB に接続する場合は上の `docker run` 例を使ってください。

```sh
go test -count=1 -race ./...
go vet ./...
docker build -t mongodb-backup-s3:integration .
go test -count=1 -tags=integration -v -timeout=20m ./tests/integration
```

結合テストは使い捨ての認証付き MongoDB レプリカセットと MinIO でバックアップ・復元を検証します。
`MONGODB_TEST_IMAGE`、`BACKUP_TEST_IMAGE`、`S3_TEST_IMAGE` でテスト用イメージを指定できます。
Docker がない環境では `MONGODB_NATIVE_INTEGRATION=1` を指定すると、新しいローカルプロセスで実行できます。
その場合は `mongod`、`minio`、`mongodump`、`mongorestore` とビルド済みの `mongodb-backup-s3` を PATH に置きます。
`MONGOD_TEST_BINARY`、`MINIO_TEST_BINARY`、`BACKUP_TEST_BINARY` で実行ファイルのパスも指定できます。
暗号化のテストには `gpg` が必要です。既存の MongoDB へ接続するテストではありません。

```sh
go build -o ./mongodb-backup-s3 ./cmd/mongodb-backup-s3
MONGODB_NATIVE_INTEGRATION=1 BACKUP_TEST_BINARY="$(pwd)/mongodb-backup-s3" \
  go test -count=1 -tags=integration -v -timeout=20m ./tests/integration
```

CI は検証のみを行い、イメージの公開は行いません。
`/run.sh`、`/backup.sh`、`/restore.sh` は対応する Go コマンドへの互換ラッパーです。
