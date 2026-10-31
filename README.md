# Astovia

Astoviaは、「忘れてた！」を防ぐためのセルフホストのタスク管理です。Web画面でタスクを管理し、スマホなどへWeb Pushで通知します。GitHub連携では、レビュー依頼と割り当てを取り込んで、PR／Issueの状態から自動で完了にします。自分へのメンションは、AI（AnthropicのAPIキー、自分のChatGPTのプラン、OpenAIのAPIキー、OpenAI互換の接続先のいずれか）が読んでやることがあるものをタスクにし、自分宛てか判断しきれないものは「タスク候補」にします。AIクライアントや外部プログラムからは、REST APIとMCP（OAuthで接続）で操作できます。Slack連携では、自分へのメンションとDM（設定でグループDMも）をAIが読んで同じようにタスクとタスク候補にし、自分の返信で用件が済んだとAIが判断したら自動で完了にします。Chatwork連携では、自分宛てのTO・返信・TO ALLとダイレクトチャットを同じようにAIが読んでタスクにし、自分に割り当てられたChatworkのタスクも取り込みます。メール連携（Gmail・Google Workspace）では、受信トレイに届いたメールをスレッドごとにAIが読んで同じようにタスクにし、自分の返信で用件が済んだら自動で完了にします。会議などの日時が決まった投稿は、Googleカレンダーに登録するタスク（予定タスク）にします。保存先はSQLiteかPostgreSQLです。GitHub・Slack・Chatwork・メール（Gmail）以外の外部サービス連携は**まだ実装していません**。

使ってみる場合は「本番で動かす」を、開発する場合は「Dockerでローカル開発する」を読んでください。

## 本番で動かす

### 公開イメージ

GitHub Container Registryで次の2つのイメージを公開しています。どちらもlinux/amd64とlinux/arm64に対応しています。

| イメージ | 内容 |
| --- | --- |
| `ghcr.io/i2bskn/astovia` | アプリ本体（画面を埋め込んだ単一バイナリ。`8080`番ポートでHTTPを待ち受けます） |
| `ghcr.io/i2bskn/astovia-migrate` | スキーマ適用用。Atlas Community Editionと、そのバージョンのスキーマ定義を含みます |

タグは`X.Y.Z`、`X.Y`（その系列の最新）、`latest`（最新の安定版）です。**本番では`X.Y.Z`を指定し、アプリとmigrateは同じバージョンにそろえてください。**

### 共通の前提

- HTTPSで公開してください。Web PushとCookieの`Secure`属性にHTTPSが必要です。`ASTOVIA_PUBLIC_URL`には利用者がブラウザで開くHTTPSのOrigin（例: `https://astovia.example.com`）を設定します。
- 同じDBに対して動かすプロセスは1つだけにしてください（「運用上の注意」を参照）。オートスケールや、デプロイ時に新旧のコンテナが並行して動く構成では、通知が重複する場合があります。AIにChatGPTのプランを使う場合は、トークンの更新が重なって接続が切れることもあります（「AIの設定」を参照）。
- `ASTOVIA_JWT_SECRET`は`openssl rand -base64 32`で一度だけ生成し、保管してください。変更すると、保存したGitHubトークン、AIのAPIキー、VAPIDの秘密鍵を復号できなくなります（「運用上の注意」を参照）。
- アプリは起動時にスキーマを更新しません。初回と、バージョンを更新するたびに、migrateイメージでスキーマを適用してください。適用前にアプリを起動すると「database schema is not initialized or incompatible」と表示して終了します。
- `.env`の値に`$`を含む場合は、値全体をシングルクォートで囲んでください（Composeはシングルクォートの中を展開しません）。接続文字列のパスワードに`@`、`:`、`/`、`?`、`#`、`%`等を含む場合はURLエンコードしてください。`.env`は`chmod 600`にしてください。

### VPSで動かす（SQLite＋Caddy）

Docker Composeが動き、80番と443番のポートを外部から受け付けるサーバーと、そのサーバーを指すドメインのA／AAAAレコードが必要です。CaddyがHTTPSの証明書を自動で取得し、アプリへ転送します。アプリのポートは外部へ公開しません。

1. 作業用のディレクトリを作り、使うバージョンのタグから設定ファイルを取得します（`vX.Y.Z`は使うバージョンに置き換えてください）。

   ```sh
   mkdir astovia && cd astovia
   for f in compose.yaml Caddyfile .env.example; do
     curl -fsSLO "https://raw.githubusercontent.com/i2bskn/astovia/vX.Y.Z/deploy/$f"
   done
   ```

2. `.env`を作成し、値を設定します。

   ```sh
   cp .env.example .env
   chmod 600 .env
   openssl rand -base64 32   # ASTOVIA_JWT_SECRETに設定する値
   ```

   `ASTOVIA_VERSION`には先頭の`v`を付けないバージョン（例: `0.1.0`）を、`ASTOVIA_DOMAIN`にはホスト名だけ（`https://`、ポート、パスを付けない。例: `astovia.example.com`）を、`ASTOVIA_JWT_SECRET`には生成した値を設定します。

3. スキーマを適用します。Atlasが表示する変更計画を確認してから承認してください。

   ```sh
   docker compose run --rm migrate
   ```

4. 初期ユーザー（オーナー）を作成します。パスワードは画面に表示されない対話入力で設定します。2人目以降は、画面の「アカウント → ユーザー管理」（サイドバー下のメールアドレス、スマホは右上のアイコンから開きます）から招待できます。

   ```sh
   docker compose run --rm app users create --email you@example.com
   ```

5. 起動し、`https://<ASTOVIA_DOMAIN>/`を開いてログインします。

   ```sh
   docker compose up -d
   ```

DBは名前付きボリューム`astovia-data`に保存します。ユーザーの招待・パスワード再設定リンクの発行・無効化は画面の「アカウント → ユーザー管理」で、画面が使えないときは`docker compose run --rm app users ...`で行います（「ユーザーを管理する」を参照）。Caddyを経由するため、ログインのレート制限はCaddyのIPにまとめて数えられます（「認証と運用上の制約」を参照）。

#### バックアップと復元（VPS）

アプリを止めてから、ボリュームの内容をホストのユーザーでコピーします。止めた状態ならSQLiteのファイルをそのままコピーしても整合します。

```sh
docker compose stop app
mkdir -p -m 700 backups
docker run --rm --user "$(id -u):$(id -g)" -v astovia-data:/data:ro -v "$PWD/backups:/backups" alpine:3.22.2 \
  sh -c 'umask 077 && d=$(date +%Y%m%d%H%M%S) && mkdir "/backups/$d" && cp -R /data/. "/backups/$d"/ && echo "backups/$d"'
```

最後に表示された`backups/<日時>`へ、設定ファイルもコピーします。

```sh
cp compose.yaml Caddyfile .env backups/<日時>/
```

バックアップにはパスワードのハッシュと暗号化した秘密が含まれ、`.env`には`ASTOVIA_JWT_SECRET`が含まれます。暗号化した値は同じ`ASTOVIA_JWT_SECRET`がないと復元後に読めないため、DBと`.env`を一緒に保管してください。ディレクトリはパーミッション700のまま保管し、サーバーの外へ持ち出す場合も同じように扱ってください。

復元はアプリを止めてから行います。

```sh
docker compose stop app
docker run --rm -v astovia-data:/data -v "$PWD/backups/<日時>:/restore:ro" alpine:3.22.2 \
  sh -c 'rm -f /data/astovia.db /data/astovia.db-journal /data/astovia.db-wal /data/astovia.db-shm && cp /restore/astovia.db* /data/ && chown -R 1000:1000 /data && chmod 755 /data && chmod 644 /data/astovia.db*'
```

前のバージョンに戻す場合は、`compose.yaml`、`Caddyfile`、`.env`もバックアップ内のものに戻してから`docker compose up -d`を実行します。

**前のバージョンに戻すときに、古いバージョンのmigrateを実行しないでください。**新しいバージョンで追加した列やテーブルを削除する計画になり、承認するとデータを失います。戻すときは、更新前に取ったバックアップを復元してから、前のバージョンのアプリを起動してください。

#### 更新する（VPS）

Slack連携を追加したバージョンへ更新するときは、`slack_connections`テーブルが増えます。Chatwork連携を追加したバージョンへ更新するときは、`chatwork_connections`テーブルが増えます。メール連携を追加したバージョンへ更新するときは、`gmail_accounts`と`google_auth_states`テーブルが増えます。AIの追加の指示を追加したバージョンへ更新するときは、`ai_instructions`テーブルが増えます。アプリは起動時にこれらのテーブルも確認するため、必ず下の手順どおり、新しいアプリを起動する前にスキーマを適用してください。

通知の送り先（Slack・メール）を追加したバージョンへ更新するときは、`notification_channels`テーブルと`notifications`の`channel`列が増え、一意のインデックス`notifications_user_dedupe_key`が`(user_id, dedupe_key)`から`(user_id, dedupe_key, channel)`に変わります。既存の通知はすべて`channel`が`webpush`になります。スキーマを適用してから新しいアプリをデプロイするまでの間、古いアプリは通知を保存できません（同期の書き込みは取り消され、次の実行でやり直されます）。適用したらすぐにデプロイしてください。下の手順のとおり、アプリを止めてから適用してデプロイするのが安全です。PostgreSQLでは、インデックスを作り直す間`notifications`への書き込みが待たされます（通知は30日で削除するため、表は小さいはずです）。

1. 新しいバージョンのタグから`compose.yaml`、`Caddyfile`、`.env.example`を別名で取得し（例: `curl -fsSL -o compose.yaml.new https://raw.githubusercontent.com/i2bskn/astovia/vX.Y.Z/deploy/compose.yaml`）、手元のファイルとの差分を確認して反映します。必須の環境変数が増えた場合は`.env`にも追加します。
2. アプリを止めます: `docker compose stop app`
3. 「バックアップと復元（VPS）」の手順でバックアップを取ります。
4. `.env`の`ASTOVIA_VERSION`を新しいバージョンに変更します。
5. スキーマを適用します: `docker compose run --rm migrate`（変更計画を確認してから承認してください）
6. 起動します: `docker compose up -d`

### コンテナサービスと外部PostgreSQLで動かす

Lightsailのコンテナサービスのようなコンテナの実行環境と、Supabaseのような外部のPostgreSQLで動かす場合の設定です。アプリのイメージ`ghcr.io/i2bskn/astovia:X.Y.Z`に次の環境変数を設定します。

| 環境変数 | 値 |
| --- | --- |
| `ASTOVIA_DB_DRIVER` | `postgres` |
| `ASTOVIA_DB_DSN` | PostgreSQLの接続URL（例: `postgres://USER:PASSWORD@HOST:5432/postgres?sslmode=require`） |
| `ASTOVIA_JWT_SECRET` | `openssl rand -base64 32`で生成した値 |
| `ASTOVIA_PUBLIC_URL` | サービスが割り当てたHTTPSのURL、またはカスタムドメインのURL |

- コンテナのポートは`8080`（HTTP）です。TLSはサービス側で終端してください。ヘルスチェックのパスは`/healthz`です。
- インスタンス数は常に1にしてください。GitHubの同期と通知はプロセス内で動くため、リクエストがない間もCPUが動く構成が必要です。リクエストのない間に停止したりCPUを絞ったりする構成では、通知が止まったり遅れたりします。
- URLが作成後に決まるサービスでは、URLが決まってから`ASTOVIA_PUBLIC_URL`を設定して再デプロイしてください。

スキーマは手元のDockerから適用します。Atlasが変更計画の計算に使う使い捨てのPostgreSQL（本番のDBとは別）もコンテナで起動します。

1. 作業用のディレクトリで、使うバージョンのタグから`compose.migrate-postgres.yaml`を取得します。

   ```sh
   curl -fsSLO https://raw.githubusercontent.com/i2bskn/astovia/vX.Y.Z/deploy/compose.migrate-postgres.yaml
   ```

2. 同じディレクトリに`.env`を作成し（`chmod 600`）、次を設定します。

   ```sh
   ASTOVIA_VERSION=X.Y.Z
   ASTOVIA_MIGRATE_URL=postgres://USER:PASSWORD@HOST:5432/postgres?sslmode=require
   # 本番のPostgreSQLのメジャーバージョン（既定17）
   ASTOVIA_DEV_POSTGRES=17
   ```

3. スキーマを適用します。Atlasが表示する変更計画を確認してから承認してください。適用対象は`public`スキーマだけです。計画が`public`以外のスキーマに触れていないことも確認してください。

   ```sh
   docker compose -f compose.migrate-postgres.yaml run --rm migrate
   docker compose -f compose.migrate-postgres.yaml down
   ```

この`.env`は本番DBの認証情報を含みます。使い終えたら削除するか、パーミッション600のまま保管してください。

初期ユーザー（オーナー）は手元のDockerから作成します。2人目以降は画面から招待できます。接続URLはシェルの環境変数に設定し、`-e ASTOVIA_DB_DSN`の形で渡します（コマンドラインに直接書かないでください）。

```sh
docker run --rm -it -e ASTOVIA_DB_DRIVER=postgres -e ASTOVIA_DB_DSN ghcr.io/i2bskn/astovia:X.Y.Z users create --email you@example.com
```

#### バックアップと復元（外部PostgreSQL）

`public`スキーマのテーブルだけを対象に、`pg_dump`のコンテナでバックアップします。`ASTOVIA_DB_DSN`はシェルの環境変数に設定しておいてください。以下の`postgres:17-alpine`の`17`は本番のPostgreSQLのメジャーバージョンに合わせてください（新しい`pg_dump`のダンプは、古いサーバーへ復元できない場合があります）。

```sh
(umask 077 && docker run --rm -e ASTOVIA_DB_DSN postgres:17-alpine \
  sh -c 'pg_dump --table="public.*" --format=custom --no-owner --no-privileges "$ASTOVIA_DB_DSN"' \
  > astovia-$(date +%Y%m%d%H%M%S).dump)
```

復元では、`public`スキーマのテーブルをすべて削除してから、ダンプの内容を入れ直します。削除と入れ直しは1つのトランザクションで行い、途中で失敗した場合は何も変更しません。`public`スキーマ自体と権限の設定、他のスキーマには触れません。

```sh
docker run --rm -i -e ASTOVIA_DB_DSN -v "$PWD/astovia-<日時>.dump:/backup.dump:ro" postgres:17-alpine sh -c '
  pg_restore --no-owner --no-privileges --file=/tmp/restore.sql /backup.dump &&
  psql "$ASTOVIA_DB_DSN" --single-transaction -v ON_ERROR_STOP=1 --quiet --output=/dev/null --file=- --file=/tmp/restore.sql' <<'SQL'
SET client_min_messages = warning;
DO $$
DECLARE r record;
BEGIN
  FOR r IN SELECT tablename FROM pg_tables WHERE schemaname = 'public' LOOP
    EXECUTE format('DROP TABLE IF EXISTS public.%I CASCADE', r.tablename);
  END LOOP;
END $$;
SQL
echo "exit code: $?"
```

終了コードが0であることを確認してから、前のバージョンのアプリをデプロイしてください。0以外の場合、DBは復元前のままです。原因を解決するまで前のバージョンをデプロイしないでください。VPSの場合と同じく、**前のバージョンに戻すときに古いバージョンのmigrateは実行しないでください。**本番で使う前に、別のDB（Supabaseなら別のプロジェクト）で復元を一度試してください。Supabaseの自動バックアップがあるプランでも、復元できる単位がプランによって異なるため、更新前には上の手順でバックアップを取ることを勧めます。

#### 更新する（外部PostgreSQL）

Slack連携を追加したバージョンへ更新するときは、`slack_connections`テーブルが増えます。Chatwork連携を追加したバージョンへ更新するときは、`chatwork_connections`テーブルが増えます。メール連携を追加したバージョンへ更新するときは、`gmail_accounts`と`google_auth_states`テーブルが増えます。AIの追加の指示を追加したバージョンへ更新するときは、`ai_instructions`テーブルが増えます。アプリは起動時にこれらのテーブルも確認するため、必ず下の手順どおり、新しいアプリを起動する前にスキーマを適用してください。

通知の送り先（Slack・メール）を追加したバージョンへ更新するときは、`notification_channels`テーブルと`notifications`の`channel`列が増え、一意のインデックス`notifications_user_dedupe_key`が`(user_id, dedupe_key)`から`(user_id, dedupe_key, channel)`に変わります。既存の通知はすべて`channel`が`webpush`になります。スキーマを適用してから新しいアプリをデプロイするまでの間、古いアプリは通知を保存できません（同期の書き込みは取り消され、次の実行でやり直されます）。適用したらすぐにデプロイしてください。下の手順のとおり、アプリを止めてから適用してデプロイするのが安全です。PostgreSQLでは、インデックスを作り直す間`notifications`への書き込みが待たされます（通知は30日で削除するため、表は小さいはずです）。

1. コンテナサービスでアプリを止めます（サービスを無効にする、またはインスタンス数を0にする）。古いアプリが新しいスキーマで動き続けることと、デプロイ中に新旧が並行して動いて通知が重複することを避けるためです。
2. 「バックアップと復元（外部PostgreSQL）」の手順でバックアップを取ります。
3. 新しいバージョンのタグから`compose.migrate-postgres.yaml`を取得し、`.env`の`ASTOVIA_VERSION`を新しいバージョンにして、スキーマを適用します（変更計画を確認してから承認してください）。
4. コンテナサービスのイメージを新しい`X.Y.Z`に変更してデプロイし、再開します。
5. 失敗して戻す場合は、古いバージョンのmigrateを実行せず、バックアップを復元してから前のバージョンをデプロイします。

#### Supabaseを使う場合

- Astovia専用のプロジェクトを作成してください。
- Data APIをオフにしてください。AstoviaはRow Level Securityを使わないため、オンのままだと`public`のテーブルをAPIキーで読める状態になる場合があります。
- 接続にはSupavisorのsessionモード（ポート5432）の接続文字列を使ってください。transactionモード（ポート6543）はプリペアドステートメントを使えないため使用できません。
- スキーマの適用時は、Atlasの計画が`public`以外のスキーマ（`auth`、`storage`等）に触れていないことを確認してから承認してください。
- `ASTOVIA_DEV_POSTGRES`と、バックアップ・復元に使う`postgres`イメージのメジャーバージョンを、プロジェクトのPostgreSQLに合わせてください。

## Dockerでローカル開発する

Docker ComposeとGNU Make、OpenSSLが必要です。この開発方法ではホスト側のGo・Node.js・pnpm・Atlas CLIは不要です。AtlasはDockerコンテナで実行します。GoはAirでソースの変更を検知して再ビルド・再起動し、React／TypeScript／CSSはViteの開発サーバーで変更を反映します。

以下はリポジトリのルートで実行してください。`compose.yaml`はAirとViteを動かすローカル開発用、`compose.test.yaml`はテスト・ビルド・コード生成・Atlas用です。`make`または`make help`で利用できるターゲットの一覧を表示します。

### 1. `.env`を作成する

```sh
make env
```

`.env.example`から`.env`を作成し、`ASTOVIA_JWT_SECRET`に`openssl rand -base64 32`で生成した署名鍵を、`LOCAL_UID`と`LOCAL_GID`にホストの`id -u`／`id -g`を設定します。ファイルのパーミッションは600です。`.env`が既にある場合は「.env already exists; nothing changed」と表示し、内容を一切変更しません。既存の`.env`を更新する場合は、`.env.example`を参考に手で追記・変更してください。

署名鍵は32バイト以上の乱数を標準のパディング付きBase64で表したものです。再起動時も同じ値を使用し、Gitへ追加しないでください。Docker Composeはシェルの環境変数またはリポジトリルートの`.env`から`ASTOVIA_JWT_SECRET`、`LOCAL_UID`、`LOCAL_GID`を展開します。`LOCAL_UID`と`LOCAL_GID`をホストのユーザーに合わせることで、コンテナから`data/`配下のDBを更新できます。省略時は`1000:1000`です。

### 2. スキーマを適用する

```sh
make migrate
```

`data/`を作成し、Atlasコンテナで`db/sqlite/schema.hcl`を`data/astovia.db`へ適用します。Atlasが表示する変更計画を確認してから承認してください（自動承認はしません）。アプリの起動時にスキーマは自動更新しません。対象はSQLiteだけです。PostgreSQLへの適用は「PostgreSQLを使用する」の手順で行ってください。

既存のDBを使っている場合も、ソースを更新してスキーマが変わったとき（例: 対応事項の`action_items`テーブル、APIトークンの`api_tokens`テーブル、MCPのOAuthの`oauth_clients`・`oauth_grants`・`oauth_tokens`テーブルの追加、GitHub連携と通知の`github_connections`・`web_push_subscriptions`・`notification_settings`・`notifications`・`instance_secrets`テーブルと`action_items`の列の追加、AIの設定の`ai_settings`テーブルの追加、Slack連携の`slack_connections`テーブルの追加、Chatwork連携の`chatwork_connections`テーブルの追加、メール連携の`gmail_accounts`・`google_auth_states`テーブルの追加、`action_items`の`importance`列の追加、AIの追加の指示の`ai_instructions`テーブルの追加、通知の送り先の`notification_channels`テーブルと`notifications`の`channel`列の追加）は、起動前にもう一度`make migrate`を実行してください。起動時のスキーマ確認はこれらのテーブルも対象にするため、OAuthのテーブルを追加する前のDBでは、migrateするまで起動しません。テーブルが不足していると、アプリは「database schema is not initialized or incompatible」と表示して起動しません。

生のコマンドは次のとおりです。

```sh
mkdir -p data
docker compose -f compose.test.yaml run --rm atlas schema apply --url 'sqlite://data/astovia.db' --to 'file://db/sqlite/schema.hcl' --dev-url 'sqlite://file?mode=memory'
```

### 3. 初期ユーザーを作成する

```sh
make user-create USER_EMAIL=alice@example.com
```

ログインIDはメールアドレスです。作成したユーザーはオーナーになります。パスワードは画面に表示されない対話入力で設定します。`USER_EMAIL`はmakeのコマンドライン引数で指定してください。シェルの環境変数として設定した`USER_EMAIL`は使用せず、未指定時と同じく何も実行せずに失敗します。make経由で使える文字は英数字と`._%+@-`だけで、`@`を含まない値や3〜254文字に収まらない値も実行前に拒否します。それ以外の記号を含むアドレスは`astovia users ... --email`を直接使ってください。形式の検証はCLIが行います。

ログインIDをメールアドレスに変更する前に作成したSQLiteのDBは、`users`表の列が異なるため起動時のスキーマ確認で失敗します。開発中のデータしか無い場合は、DBファイルを削除して`make migrate`で作り直し、`make user-create USER_EMAIL=...`でユーザーを作り直してください。自動移行は用意していません。旧DBに対して`make migrate`を実行すると、`username`列の削除と`email`列の追加の計画が表示されます。データが残らないため承認せず、DBファイルを作り直してください。

ユーザー操作は`docker compose run --rm app go run ./cmd/astovia users ...`で、イメージ内の古いバイナリではなく現在のソースから実行します。そのため、CLIのコードを変更してもイメージの再ビルドは不要です。ユーザー操作CLIには署名鍵は不要ですが、Airから起動する`serve`では必須です。

### 4. 起動・確認・停止する

```sh
make up
make logs
```

[http://localhost:5173/](http://localhost:5173/)を開いてログインしてください。ホストへ公開するのはViteの`127.0.0.1:5173`だけです。Goはコンテナ内の`0.0.0.0:8080`で待ち受け、Viteが`/auth`・`/api`・`/healthz`・`/mcp`・`/oauth`・`/.well-known`を転送します。`/consent`（OAuthの同意ページ）はWeb画面として表示します。ブラウザからのOriginは変更せず、Go側の公開URLも`http://localhost:5173`にそろえます。`localhost`と`127.0.0.1`は異なるOriginのため、ブラウザでは`localhost`を使用してください。

停止する場合は次を実行します。コンテナは削除せずに停止します（`docker compose stop`）。

```sh
make stop
```

データはホストの`./data`に保存され、コンテナの再ビルドや削除では消えません。通常のGo・TypeScript／TSX・CSSの変更は、ソースの読み取り専用マウントを通じて自動反映されます。Goの再起動中はAPIが一時的に応答できない場合があります。画面側はReact Fast Refreshで更新し、ページ全体を再読み込みした場合は既存のRefresh処理でログイン状態を復元します。

Dockerのファイル通知差異を避けるため、AirとViteのファイル監視にはポーリングを使用します。Goのビルドキャッシュ（`GOCACHE=/tmp/go-build`）は、`app`の`/tmp`にマウントしたDockerの名前付きボリューム`go-build-cache`に保存し、`docker compose run --rm`を繰り返しても再利用します。Viteのキャッシュは開発イメージ内の`node_modules`に保存します。ホスト側で事前に生成した`web/dist`や`node_modules`は不要です。

依存関係の変更、DockerfileやCompose設定の変更には、再ビルドして起動し直してください。`.air.toml`等の監視設定を変更した場合も、開発プロセスを再起動してください。

```sh
docker compose up -d --build
```

この構成はローカル開発用です。本番公開の設定やデプロイは含みません。スキーマの適用は引き続きAtlasで明示的に行い、ホットリロードではDBを更新しません。

`Dockerfile`の既定の最終段は、画面を埋め込んだ静的バイナリとCA証明書だけを含む非root実行の`scratch`イメージです。`docker build -t astovia .`で作成できます。ローカルComposeは、この最終段ではなく開発専用の`development`／`frontend-development`段を使用します。`migrate`段は公開用のスキーマ適用イメージです（「本番で動かす」を参照）。

## ネイティブバイナリをビルドする

コンテナを使わずにアプリ本体を直接起動する場合、ホストにGo 1.26.4以降が必要です。`make build`はフロントエンドのビルドにNode.js 22のDockerコンテナを使用します。フロントエンドをホスト側で開発する場合はNode.js 22とpnpm 10.28.1も用意してください。

```sh
make build
```

`bin/astovia`にホスト向けバイナリを生成します。テストは実行しません（`make check`で別途実行します）。埋め込む画面の`web/dist`は、存在しない場合と`web/src`等のソースが更新された場合だけビルドします。フロントエンドの生成物はGit管理対象外なので、取得直後にはビルドが必要です。AIの認証情報は起動に不要です。

### SQLiteで直接起動する

リポジトリのルートで実行してください。SQLiteの相対パスは、プロセスの作業ディレクトリを基準に解決します。直接起動はホストにツールをそろえる前提のため、この手順ではホスト側のAtlas CLIを使用します。

```sh
mkdir -p data
atlas schema apply --url 'sqlite://data/astovia.db' --to 'file://db/sqlite/schema.hcl' --dev-url 'sqlite://file?mode=memory'
./bin/astovia users create --email alice@example.com
# 初回に保存した署名鍵をシェルのASTOVIA_JWT_SECRETへ設定してから起動します。
./bin/astovia serve
```

Docker起動手順で同じDBとユーザーを作成済みの場合、作成操作を繰り返す必要はありません。直接起動するアプリは`.env`を自動で読み込みません。必要な値をシェルまたは実行環境の環境変数として設定してください。

既定値は次のとおりです。

| 環境変数 | 既定値 |
| --- | --- |
| `ASTOVIA_ADDR` | `127.0.0.1:8080`（Dockerイメージでは`0.0.0.0:8080`） |
| `ASTOVIA_PUBLIC_URL` | `http://localhost:8080` |
| `ASTOVIA_DB_DRIVER` | `sqlite` |
| `ASTOVIA_DB_DSN` | `file:./data/astovia.db` |
| `ASTOVIA_JWT_SECRET` | なし。`serve`で必須 |
| `ASTOVIA_GOOGLE_CLIENT_ID` | なし。`ASTOVIA_GOOGLE_CLIENT_SECRET`と両方設定するとメール連携が使える（「メール連携（Gmail・Google Workspace）」を参照） |
| `ASTOVIA_GOOGLE_CLIENT_SECRET` | なし。`ASTOVIA_GOOGLE_CLIENT_ID`と両方設定する |
| `ASTOVIA_ACCESS_TEAM_DOMAIN` | なし。`ASTOVIA_ACCESS_CLIENT_ID`、`ASTOVIA_ACCESS_CLIENT_SECRET`と3つとも設定すると、ログインがCloudflare Accessになる（「ログイン方式（パスワード / Cloudflare Access）」を参照） |
| `ASTOVIA_ACCESS_CLIENT_ID` | なし。Cloudflare AccessのSaaSアプリのClient ID |
| `ASTOVIA_ACCESS_CLIENT_SECRET` | なし。Cloudflare AccessのSaaSアプリのClient secret。ログに出さない |
| `ASTOVIA_MAIL_PROVIDER` | なし。`cloudflare`か`resend`（大文字小文字は区別しない）を設定すると、通知をメールでも送れる（「メールでの通知」を参照）。空のまま他の`ASTOVIA_MAIL_*`か`ASTOVIA_CLOUDFLARE_ACCOUNT_ID`を設定すると起動エラー |
| `ASTOVIA_MAIL_API_KEY` | なし。メールの送信サービスのAPIキー（CloudflareはEmail Serviceの送信の権限を持つAPIトークン）。ログに出さない |
| `ASTOVIA_MAIL_FROM` | なし。送信元のメールアドレス（表示名を付けない`notify@example.com`の形） |
| `ASTOVIA_MAIL_FROM_NAME` | なし。送信元の表示名（Resendだけ。100文字まで） |
| `ASTOVIA_CLOUDFLARE_ACCOUNT_ID` | なし。Cloudflare Email Serviceを使うときのアカウントID（32桁の16進数）。`resend`では設定しない |
| `ASTOVIA_AI_ALLOW_PRIVATE_ENDPOINTS` | `false`。`true`（または`1`）にすると、AIのOpenAI互換の接続先に`http`と非公開アドレスを使える（「OpenAI互換の接続先」を参照。リスクを読んでから設定する）。`true`・`1`・`false`・`0`・空以外（大文字小文字は区別しない）は起動エラー |

`compose.yaml`では、署名鍵とGoogleのOAuthクライアント（メール連携）とUID／GIDだけを外部から展開し、待ち受けアドレス・公開URL・SQLite接続先はファイル内の固定値を使用します。Composeの公開URLは`http://localhost:5173`で、直接起動時の既定値とは異なります。

ホスト側でViteを使う場合は`web`で`pnpm dev`を実行し、Go側の`ASTOVIA_PUBLIC_URL`も`http://localhost:5173`に設定してください。ViteのAPI転送先は既定で`http://localhost:8080`です。開発時だけ`ASTOVIA_DEV_API_URL`で転送先、`ASTOVIA_DEV_POLLING=true`でポーリング監視を指定できます。

## PostgreSQLを使用する

以下はアプリを直接起動する場合の設定です。`compose.yaml`はSQLiteを使用する構成です。この手順ではホスト側のAtlas CLIを使用します。コンテナで動かす場合は「本番で動かす」を参照してください。

`ASTOVIA_DB_DRIVER=postgres`と、PostgreSQLの接続URLを`ASTOVIA_DB_DSN`へ明示的に設定します。PostgreSQLの接続先を省略してもSQLiteへは切り替わりません。起動やユーザー操作の前に、同じ接続先へAtlasで`db/postgres/schema.hcl`を適用してください。

Atlasの`--dev-url`には、アプリのDBではなく、**別の使い捨て開発用DB**を指定します。

```sh
export ASTOVIA_DB_DRIVER=postgres
export ASTOVIA_DB_DSN='postgres://USER:PASSWORD@HOST/DATABASE?sslmode=require'
atlas schema apply --url "$ASTOVIA_DB_DSN" --to file://db/postgres/schema.hcl --dev-url 'docker://postgres/17/dev?search_path=public'
```

更新時は変更計画を一つずつ確認し、永続データをバックアップしてから適用してください。DBドライバーの設定変更だけでは、既存データは別のDBへ移行されません。

## ユーザーを管理する

ユーザーの権限はオーナー（`owner`）とメンバー（`member`）の2種類です。タスク・連携（GitHub・Slack・AI）・通知などのデータはユーザーごとに別で、オーナーも通常の画面では他のユーザーのタスクを見られません。ただし、オーナーはパスワード再設定リンクを発行し、それを自分で開けばそのユーザーとしてログインできます。オーナーは完全に信頼された立場として扱ってください。公開の新規登録画面とメールの送信はありません。

### 画面で管理する（オーナー）

オーナーは「アカウント → ユーザー管理」で次の操作ができます（メンバーにはこの画面は表示されず、APIも403で拒否します）。

- 招待: メールアドレスと権限を決めて招待リンク（`/invite#…`）を作ります。リンクは7日間有効で、表示は作成時の1回だけです。コピーして本人に渡してください。リンクを開いた人がパスワードを決めるとアカウントができ、そのままログインします。同じメールアドレスへ招待し直すと、前の招待リンクは使えなくなります。既にユーザーがいるメールアドレス（無効化中を含む）は招待できません。
- 有効な招待の一覧と取り消し。
- 権限の変更（オーナーにする／メンバーにする）。データは消えません。メンバーにしたユーザーのログインは続き、次の操作からユーザー管理が使えなくなります。そのユーザーが発行した有効な招待とパスワード再設定リンクは取り消します。
- 無効化と再開。無効化は次のCLIの`disable`と同じものを削除し、そのユーザーが発行した招待と再設定リンク、そのユーザー宛ての再設定リンクも取り消します。タスクと通知の設定は残ります。再開しても削除したものは戻りません（以前のパスワードでログインできます）。
- パスワード再設定リンク（`/reset-password#…`）の発行。リンクは24時間有効で、表示は発行時の1回だけです。発行し直すと、そのユーザーの前のリンクは使えなくなります。再設定は次のCLIの`reset-password`と同じものを削除し、他の端末からはログアウトされます。リンクで再設定したユーザーの画面には、日時と発行したオーナーのメールアドレスを14日間表示します（表示するのは直近の1件だけです。閉じるとそのブラウザでは表示しません）。

有効なオーナーが常に1人以上いるように、最後の有効なオーナーをメンバーにする操作は拒否します。オーナーは自分自身を無効化できません。他にオーナーがいれば、自分をメンバーにすることはできます。

リンクのトークンはURLのフラグメント（`#`以降）に置くため、サーバーのアクセスログや`Referer`には出ません。画面は読み取った直後にURLから取り除きます。DBにはトークンのハッシュだけを保存します。リンクを受け取った人以外に見せないでください。

### CLIで管理する

選択したDBにアクセスできる運用者は、CLIでも操作できます。初期ユーザーの作成と、画面が使えないときの復旧に使います。

Dockerで起動している場合は、同じCompose構成で操作します。現在のソースから`go run`で実行するため、イメージの再ビルドは不要です。

```sh
make user-create USER_EMAIL=alice@example.com
make user-reset-password USER_EMAIL=alice@example.com
make user-disable USER_EMAIL=alice@example.com
make user-enable USER_EMAIL=alice@example.com
make user-role USER_EMAIL=alice@example.com USER_ROLE=owner
```

ネイティブバイナリでは次のように実行します。

```sh
./bin/astovia users create --email alice@example.com [--role owner|member]
./bin/astovia users reset-password --email alice@example.com
./bin/astovia users disable --email alice@example.com
./bin/astovia users enable --email alice@example.com
./bin/astovia users role --email alice@example.com --role owner|member
```

`users create`はオーナーを作成します。メンバーにするときは`--role member`を指定します（`--role`は`create`と`role`だけが受け付けます）。`users role`は既存のユーザー（無効化中を含む）の権限を`--role`の値に変えます。`--role`は省略できず、パスワードは聞きません（`--password-stdin`は受け付けません）。存在しないメールアドレスはエラー（`user not found`）です。メンバーにすると、画面の「メンバーにする」と同じく、そのユーザーが発行した有効な招待とパスワード再設定リンクを取り消します（セッションとAPIトークンは残します）。最後のオーナーの規則は適用しません。`users enable`は無効化したユーザーを再開し（`--password-stdin`は受け付けません）、無効化で削除したものは戻りません。CLIの`disable`には最後のオーナーの規則を適用しません（運用者の最終手段のため）。CLIで最後のオーナーを無効化すると、画面からユーザー管理ができなくなります。その場合は`users create`で新しいオーナーを作るか、`users enable`で再開して戻してください。

パスワードは端末の表示を無効にして読み取ります。非対話で実行する場合は`--password-stdin`を指定し、標準入力から1行で渡してください。末尾の改行1つ（`\n`、`\r\n`、`\r`）は除去します。複数行の入力と75バイトを超える入力は拒否します。パスワードをコマンドライン引数には含めません。Composeへパイプで渡す場合は、`docker compose run --rm -T app go run ./cmd/astovia users create --email alice@example.com --password-stdin`のように`-T`を付けて実行します。

メールアドレスは前後のASCII空白を除去して小文字に変換します（ローカル部も小文字にします）。ASCIIのみで254文字以内、`@`より前のローカル部は64文字以内です。形式はHTMLの`input type=email`と同じ規則で、`alice@localhost`のようにドットを含まないドメインも受け付けます。国際化ドメイン名はUnicodeのままでは受け付けないため、punycode（`xn--…`）で指定してください。確認メールは送信しません。パスワードはUTF-8で12〜72バイトです（画面の招待・再設定も同じです）。同じメールアドレス（大文字・小文字の違いを含む）の作成は拒否します。パスワード再設定とユーザー無効化は全セッションを失効させ、そのユーザーのAPIトークンと、OAuthで接続したAIクライアント（認可とトークン）、Web Pushの購読（通知を受け取る端末）、Slackへの通知のWebhook、AIの設定（登録したAPIキー）とAIの追加の指示、未送信の通知をすべて削除し（通知の種類ごとの送り先の選択は残します）、そのユーザーが発行した招待と再設定リンク、そのユーザー宛ての再設定リンクを取り消します。ユーザー無効化はGitHub連携とSlack連携とChatwork連携とメール連携のGoogleアカウントも削除します（パスワード再設定では残します。「GitHub連携と通知」「Slack連携」「Chatwork連携」「メール連携（Gmail・Google Workspace）」を参照）。パスワード再設定とユーザー無効化は、途中のGoogleアカウントの接続の手続きも取り消します。再設定によって無効化済みユーザーが再び有効になることはありません。CLIはサーバーと同じDB設定を使い、JWT署名鍵は必要としません。

## ログイン方式（パスワード / Cloudflare Access）

管理画面のログインは、起動時の環境変数で「パスワード」（既定）か「Cloudflare Access」のどちらか一方に決まります。両方を同時に有効にすることはできません。

Cloudflare Accessの方式では、AstoviaがCloudflare AccessのSaaSアプリケーション（OIDC）のクライアントになります。ログイン画面の「Cloudflare Access でログイン」からAccessのログインに移り、戻ってきたIDトークン（RS256）をAstoviaが自分で検証して、`email`クレームをユーザーのメールアドレスに対応づけます。ログイン後のセッション（Access JWTとRefresh Cookie）、`/api/v1`、APIトークン、MCP、MCPのOAuthは、パスワードの方式と同じです。

| 環境変数 | 内容 |
| --- | --- |
| `ASTOVIA_ACCESS_TEAM_DOMAIN` | Accessのチームドメイン（`https://<チーム名>.cloudflareaccess.com`。末尾の`/`は1つまで取り除きます） |
| `ASTOVIA_ACCESS_CLIENT_ID` | SaaSアプリのClient ID |
| `ASTOVIA_ACCESS_CLIENT_SECRET` | SaaSアプリのClient secret。ログに出しません |

- 3つはそろえて設定するか、どれも設定しません。一部だけ、形式の誤り、前後の空白は起動エラーになります（CLIも同じ検証をします）。Accessの方式では`ASTOVIA_PUBLIC_URL`がHTTPSである必要があります（ログイン中の状態を持つCookieに`Secure`と`__Host-`を使うため）。
- 誰がログインできるかは、Cloudflare側のAccessのポリシーだけで決まります。初めてログインしたメールアドレスは、メンバー（`member`）として自動で作成されます。ポリシーは許可するメールアドレスかAccessグループで書いてください。広くすると、ログインした全員がメンバーとして作成されます。
- アプリに許可するIdP（ログイン方法）は、メールアドレスの所有を確かめるもの（One-time PIN、Google、GitHubなど）に限ってください。任意のメールアドレスを名乗れるIdPを許可すると、そのメールアドレスのユーザー（オーナーを含む）としてログインされるおそれがあります。
- メールアドレスは大文字・小文字を区別せずに照合します。AstoviaはIDトークンの`sub`と`email_verified`を見ません。このアプリのIDトークンを発行するのはCloudflare Accessだけで、Accessがログインに使ったメールアドレスを`email`に入れるためです。
- 無効化したユーザーは、Accessでログインしても有効に戻らず、ログインに失敗します。
- 人を外すときは、Accessのポリシーやグループから外したうえで、Astoviaでも無効化してください。Accessで止まるのはブラウザの新しいログインだけで、ログイン済みのAstoviaのセッション（最長7日）、APIトークン、MCPの接続（OAuth）はAccessを通らないため、無効化するまで使えます。
- 方式を切り替えても、既存のセッションはそのまま残ります。切り替えた後も、期限まではログインしたままです。
- Accessの方式では、パスワードでのログイン、招待、パスワード再設定は使えません（APIは409 `password_auth_disabled`を返し、何も変更しません）。招待とパスワード再設定のリンクの画面は、使えない旨だけを表示します。既存の招待の一覧と取り消し、権限の変更、無効化と再開はそのまま使えます。招待が残っているメールアドレスでAccessからログインした場合、招待の権限は使われずにメンバーとして作成され、招待は使われないまま一覧に残ります。`users reset-password`も使う意味がありません（APIトークン、MCPの接続、AIの設定などが削除されるだけです）。
- Accessで作成したユーザーには、どのパスワードにも一致しない値をパスワードハッシュとして保存します。パスワードの方式に戻した場合、そのユーザーは`users reset-password`でパスワードを設定するまでログインできません。
- 「ログアウト」はAstoviaのセッションだけを消します。Accessのセッションは残るため、次のログインではIdPの画面を通らないことがあります。共有の端末や別のアカウントに切り替えるときは、「アカウント」画面の「Cloudflare Access からもログアウト」を使ってください（Astoviaのログアウトに成功してから、Accessのログアウト画面へ移ります）。
- ログインに失敗すると`/signin-error`に理由を表示します。ログインを始める画面（MCPの同意画面など）のURLが長すぎる場合（クライアントの`state`などが非常に長い場合）は、ログイン後に元の画面へ戻れないため、ログインを始めずにその旨を表示します。
- MCPのOAuth（AIクライアントの接続）はAccessの方式でも同じ手順で使えます。同意画面でのログインがCloudflare Accessになります。
- `/auth/access/login`と`/auth/access/callback`にはレート制限を付けません（Caddyなどのリバースプロキシの背後では全員が同じIPになり、正規のログインまで止めてしまうため）。

### 最初のオーナー

Accessの方式で最初にログインする前に、自分をオーナーとして作成してください。パスワードは使われませんが、12文字以上の入力が必要です。

```sh
docker compose run --rm app users create --email you@example.com
```

先にAccessでログインしてメンバーとして作成された場合は、権限をオーナーに変えます。

```sh
docker compose run --rm app users role --email you@example.com --role owner
```

開発用のComposeでは`make user-create USER_EMAIL=…`と`make user-role USER_EMAIL=… USER_ROLE=owner`でも実行できます。

### Cloudflareの設定（手作業）

1. Zero Trustのチームを作り、IdP（One-time PIN、Google、GitHubなど、メールアドレスの所有を確かめるもの）を追加します（作成済みなら不要）。
2. Accessのアプリケーション（SaaS、OIDC）を作ります。リダイレクトURLは`https://<ASTOVIA_PUBLIC_URLのホスト>/auth/access/callback`、スコープは`openid`と`email`、PKCEを有効にし、ポリシーでログインを許可するメールアドレスかAccessグループを指定します。Client IDとClient secretを控えます。
3. `https://<チーム名>.cloudflareaccess.com/cdn-cgi/access/sso/oidc/<Client ID>/.well-known/openid-configuration`を開き、`issuer`がこのURLから`/.well-known/openid-configuration`を除いたものであること、`authorization_endpoint`、`token_endpoint`、`jwks_uri`が`<issuer>/authorization`、`<issuer>/token`、`<issuer>/jwks`であること、署名にRS256、トークンエンドポイントの認証に`client_secret_basic`が使えることを確かめます。AstoviaはこのURLを読まず（discoveryは行いません）、これらのURLを決め打ちで使います。
4. `.env`に`ASTOVIA_ACCESS_TEAM_DOMAIN`、`ASTOVIA_ACCESS_CLIENT_ID`、`ASTOVIA_ACCESS_CLIENT_SECRET`を設定し、「最初のオーナー」の手順を済ませてから、`docker compose up -d`で再起動します。
5. 確認: `https://<ASTOVIA_DOMAIN>/`を開くと「Cloudflare Access でログイン」が表示され、ログイン後にタスクの一覧が表示されます。

## 認証と運用上の制約

- Access JWTの有効期間は最大10分で、ブラウザのメモリだけに保持します。Refreshセッションは作成から7日で失効し、ローテーションでは延長しません。
- Refresh Tokenは乱数で生成し、DBにはハッシュのみを保存します。Cookieは`HttpOnly`、`SameSite=Strict`、`Path=/auth`とし、HTTPSでは`Secure`も付けます。再利用検出のために使用済みトークンのハッシュを保持します。現時点ではセッション履歴の削除コマンドがないため、セッションや更新の回数に応じて保存量が増えます。
- 使用済みRefresh Tokenを再利用すると、そのセッションを失効させます。対応ブラウザではWeb Locksでタブ間の認証操作を直列化します。Web Locksがない環境では、同時操作によって再ログインが必要になる場合があります。
- 非公開APIへのリクエストごとに、DB上のユーザーとセッションが有効か確認します。ログアウト、パスワード再設定、ユーザー無効化、再利用検出の後は、発行済みJWTも次の検証から拒否します。実行中のリクエストを遡って取り消すことはできません。
- ブラウザ向け認証POSTは、設定されたOriginとの完全一致、`application/json`、JSONオブジェクトを必須とします。RefreshとLogoutの本文は`{}`です。コマンドラインからのリクエストも、Originがなければ拒否します。Refresh失敗時にはCookieを消さず、遅れて返った失敗応答が新しいCookieを削除することを防ぎます。
- 招待とパスワード再設定リンクの4つの公開API（確認・受諾・完了）は、ログインとは別に、4つ合わせて接続元IPごとに1分間で10回までです。数え方はログインと同じです。
- ログインは接続元IPごとに1分間で10回までです。IPv6は`/64`単位でまとめて数え、IPv4を埋め込んだIPv6はIPv4として扱います。記録件数に上限（4096件）のあるプロセス内テーブルを使用し、転送ヘッダーは信頼しません。上限に達した場合は新しい接続元を拒否せず、制限中でない最も古い記録（すべて制限中なら最も古い記録）を削除して受け付けます。このため、上限を超える数の接続元を用意できる攻撃者に対しては、毎分の総試行回数に上限がなくなります。実質的な上限はbcrypt（コスト12）のCPU負荷で、パスワードは12バイト以上を必須としています。全利用者がログインできなくなる事態を避けることを優先しています。リバースプロキシ配下の利用者はプロキシのIPにまとめて制限され、複数インスタンス間では制限を共有しません。利用範囲を広げる際は、手前のプロキシ等にも適切なレート制限を設定してください。
- リバースプロキシを使用する場合は、`ASTOVIA_PUBLIC_URL`にHTTPSのOriginを設定します。パス、認証情報、クエリ、フラグメントは指定できません。プロキシでTLSを終端し、バックエンドへの直接接続を制限してください。HTTPの公開URLはループバック開発用途だけに限ります。
- ホスト名はASCIIのDNS名か明示的なIPアドレスを使用します。国際化ドメイン名はpunycodeで指定してください。数値ホストの省略表記、スコープ付きIPv6、IPv4を埋め込んだIPv6は拒否します。DNS名の大文字・小文字、標準ポート、ポートの数値表記、IPv6の表記は設定読込時に正規化し、Origin検証とJWT issuerで共用します。以前の正規化されていないissuerで発行したトークンは再ログインが必要です。正規化済みの設定には影響しません。

Web画面は`/`（タスク）、`/candidates`（タスク候補）、`/auto`（自動で処理したもの）、`/settings`（設定）、`/account`（アカウント。オーナーには`/account/users`のユーザー管理もあります）、`/tasks/{id}`（タスクの詳細）で開きます。以前の`/settings/account`と`/settings/users`は`/account`と`/account/users`に移ります。招待リンクは`/invite`、パスワード再設定リンクは`/reset-password`で開きます。Cloudflare Accessでのログインに失敗したときは`/signin-error`を表示します。以前の版の`/admin/`以下のURL（ブックマークや送信待ちの通知を含む）は、対応する新しいURLへリダイレクトします。権限はオーナーとメンバーの2種類だけで、共有Inboxや、通常の画面で他ユーザーの非公開情報を見る権限はありません（オーナーによるパスワード再設定リンクの扱いは「ユーザーを管理する」を参照）。本番での動かし方は「本番で動かす」を参照してください。

## APIトークン

APIトークンは、AIクライアントや外部プログラムが、本人の対応事項を操作し、本人が連携したChatworkとSlackとGmailを読むための長期間有効なBearerトークンです。

- 管理画面の「API トークン」で名前（1〜100文字）を付けて発行します。トークン（`ast_`で始まる47文字）は発行した直後に一度だけ表示し、再表示できません。DBには検証用のハッシュだけを保存します。
- 1ユーザーあたり50件まで発行できます。有効期限はなく、管理画面で削除すると即座に失効します。一覧には名前、作成日時、最終使用日時（1分単位で記録）を表示します。
- パスワード再設定とユーザー無効化を行うと、そのユーザーのAPIトークンはすべて削除されます。
- AIクライアントの接続にはOAuth（次の「MCP」「OAuth」を参照）を推奨します。APIトークンは、OAuthに対応していないクライアントやスクリプトのための補助手段です。
- 使える範囲は`/mcp`と`/api/v1/action-items*`だけです。`/api/v1/me`とトークン管理（`/api/v1/api-tokens*`）はAPIトークンでは呼べず、管理画面のログイン（Access JWT）が必要です。発行時はログイン中のセッションが有効かをDB上で再確認します。

## MCP

`/mcp`はMCPのStreamable HTTPエンドポイントです。ステートレスで、`POST`だけを受け付けます（`GET`／`DELETE`は405。SSEストリームとセッションは提供しません）。

AIクライアントは、URLを入力するだけでOAuthによる認可を経て接続できます。トークンを手で貼る必要はありません。URLは`ASTOVIA_PUBLIC_URL`に`/mcp`を付けたものです（開発環境（Docker）では`http://localhost:5173/mcp`）。管理画面の「接続中の AI クライアント」にも表示します。

- Claude Code: 次のコマンドで追加し、Claude Codeの中で`/mcp`を実行して認証します。ブラウザでAstoviaの同意ページが開くので、ログインして「許可する」を押してください。

  ```sh
  claude mcp add --transport http astovia https://astovia.example.com/mcp
  ```

- Claude（Web・Desktop）のカスタムコネクタやChatGPT: コネクタのURLに`<ASTOVIA_PUBLIC_URL>/mcp`を入力します。これらはAnthropic・OpenAIのサーバーからAstoviaへ接続するため、インターネットから到達できるHTTPSの公開URLが必要です。ローカル開発環境（`http://localhost:5173`）には接続できません。

APIトークンも引き続き使えます。OAuthに対応していないクライアントでは、管理画面で発行したトークンを`Authorization: Bearer <APIトークン>`で送ってください。

```sh
claude mcp add --transport http astovia http://localhost:5173/mcp --header "Authorization: Bearer ast_..."
```

認証は`Authorization: Bearer`で、OAuthのアクセストークン（`aso_`で始まる）かAPIトークン（`ast_`で始まる）を受け付けます。Access JWTとOAuthのリフレッシュトークンは受け付けません。認証できない場合は401と次の`WWW-Authenticate`を返します。MCPクライアントはこれを手がかりにOAuthを始めます。

- トークンがない場合: `Bearer resource_metadata="<ASTOVIA_PUBLIC_URL>/.well-known/oauth-protected-resource/mcp", scope="mcp"`
- トークンを送ったうえで無効だった場合: `Bearer error="invalid_token", resource_metadata="…", scope="mcp"`

提供するツールは次の15です。`action_items_*`の4つは、常にトークンの持ち主の対応事項を操作し、入力の検証とエラーの内容はREST APIと同じです。`chatwork_*`の4つは、トークンの持ち主が連携したChatwork（「Chatwork連携」を参照）を読むだけで、Chatworkへの書き込み（投稿・既読化・タスクの操作）はしません。Chatworkを連携していなければ使えません。`slack_*`の4つは、トークンの持ち主が連携したSlack（「Slack連携」を参照）を、連携に登録したUser OAuth Tokenで読むだけで、Slackへの書き込み（投稿・既読・リアクション）はしません。Slackを連携していなければ使えません。`gmail_*`の3つは、トークンの持ち主が連携したGoogleアカウント（「メール連携（Gmail・Google Workspace）」を参照）のGmailを、連携に登録したトークン（`gmail.readonly`）で読むだけで、Gmailへの書き込み（既読・ラベル・送信・下書き）はしません。Googleアカウントを接続していなければ使えません。検証エラーや存在しない項目は、ツール結果のエラー（`isError`、本文は`<code>: <message>`）として返します。

| ツール | 内容 |
| --- | --- |
| `action_items_create` | 対応事項を作成する。引数はRESTの作成本文と同じ（`parent_id`の`null`は省略と同じで、親なし） |
| `action_items_list` | 一覧を返す。引数は`status`（`open`／`candidate`／`done`／`dismissed`）、`limit`（1〜100、既定50）、`cursor`（いずれも省略可）。`parent_id`を渡すと、その親の子タスクを作成日時の古い順に返す（`status`・`limit`・`cursor`とは併用できない） |
| `action_items_get` | `id`で1件返す |
| `action_items_update` | `id`と、RESTの更新本文と同じキー（`version`を含む）で更新する。`parent_id`の`null`は親から外す |
| `chatwork_rooms_list` | 自分のChatworkのルームを、最終更新の新しい順に返す（`{rooms: [{room_id, name, type, unread_num, mention_num, last_update_at, url}], total}`）。引数は`query`（ルーム名に含まれる語、1〜100文字）、`type`（`my`／`direct`／`group`）、`limit`（1〜200、既定50）（いずれも省略可） |
| `chatwork_messages_list` | `room_id`のルームの最新のメッセージを新しい順に返す（`{room_id, room_url, messages, matched, fetched}`）。引数は`query`（本文か送信者名にすべて含まれる語、1〜100文字）、`limit`（1〜100、既定30）（いずれも省略可） |
| `chatwork_message_get` | `room_id`と`message_id`でメッセージを1件返す。対応事項の`refs`の`conversation_id`と`message_id`から元のメッセージを引くときに使う |
| `chatwork_messages_search` | 複数のルームの最新のメッセージから、`query`（必須）の語をすべて含むものを新しい順に返す（`{query, messages, matched, searched_rooms, skipped, missing_room_ids, note}`）。対象は`room_ids`（1〜20件）か、最終更新の新しい`rooms`件（1〜20、既定10。`type`で絞れる）。`limit`は1〜100、既定30 |
| `slack_messages_search` | Slackの検索（`search.messages`）で、`query`（必須、1〜500文字）に合う発言を新しい順に返す（`{query, messages, page, pages, total}`）。`query`はSlackの検索構文のまま渡す（`in:#general`、`from:<@U…>`、DMは`in:<@U…>`など）。`count`（1〜100、既定20）、`page`（1〜100、既定1） |
| `slack_conversations_list` | 参加中のチャンネル・プライベートチャンネル・DM・グループDMを、Slackの返した順に返す（`{conversations: [{id, name, type, url}], next_cursor}`。アーカイブ済みは除く）。引数は`types`（`public_channel`／`private_channel`／`im`／`mpim`の配列、既定はすべて）、`limit`（1〜200、既定100）、`cursor`（いずれも省略可）。1ページの件数は`limit`より少ない（0件のこともある）ことがあるため、`next_cursor`が空になるまで`cursor`に渡して続きを読む |
| `slack_messages_list` | `channel`の会話の最近の発言を新しい順に返す（`{channel_id, messages, has_more, next_cursor}`）。スレッドの返信は含まない。引数は`limit`（1〜100、既定30）、`oldest`・`latest`（オフセット付きのRFC 3339）、`cursor`（いずれも省略可） |
| `slack_thread_get` | `channel`と`ts`でスレッド全体を古い順に返す（`{channel_id, thread_ts, messages, has_more, next_cursor, partial}`）。`ts`が返信なら親から読み直し、スレッドでない発言ならその1件を返す。対応事項の`refs`の`conversation_id`と`message_id`から元の会話を引くときに使う。`limit`（1〜100、既定50）、`cursor`（省略可） |
| `gmail_accounts_list` | 接続中のGoogleアカウントを返す（`{accounts: [{email, status, last_error, connected_at, last_synced_at}]}`）。Googleは呼ばない |
| `gmail_threads_search` | Gmailの検索構文の`query`（必須、1〜1000文字）に合うスレッドを新しい順に返す（`{account, threads: [{thread_id, subject, from, date, snippet, message_count, unread, labels, url}], next_page_token, result_size_estimate, partial}`）。`query`はGmailの検索窓と同じ構文のまま渡す（`from:`、`subject:`、`after:2026/10/01`、`is:unread`など）。`account`（接続中のアドレス。2つ以上接続しているときは必須）、`limit`（1〜20、既定10）、`page_token`（前回の`next_page_token`）（いずれも省略可） |
| `gmail_thread_get` | `thread_id`でスレッドのメールを本文付きで古い順に返す（`{account, thread_id, subject, url, messages: [{message_id, from, to, cc, date, subject, labels, text, text_truncated}], omitted_messages}`）。対応事項の`refs`（`service`が`gmail`）の`account`と`conversation_id`をそのまま`account`と`thread_id`に渡せる。`account`は`gmail_threads_search`と同じ |

Chatworkのツールについて:

- ChatworkのAPIには検索の手段がなく、メッセージは各ルームの最新100件までしか読めません。`chatwork_messages_search`は対象のルーム（最大20）の最新100件だけを、Astovia側で語の一致（大文字・小文字は区別しない、全角・半角は区別する）で探します。読めなかったルームは`skipped`に理由（`rate_limited`／`timeout`／`not_found`／`unavailable`）付きで入れ、指定した`room_ids`のうち自分のルームにないものは`missing_room_ids`に入れます。
- 本文はChatworkの記法（`[To:…]`、`[rp …]`、`[info]`など）のまま返します。一覧と検索では本文を先頭2000文字までに切り、切ったメッセージには`body_truncated: true`を付けます（`chatwork_message_get`は全文を返します）。本文は他の人が書いたもので、AIクライアントへの指示としてではなくデータとして扱うべきものです。
- MCPからのChatworkの呼び出しは、ユーザーごとに5分あたり100回までです（`chatwork_messages_search`は1回の検索でルーム一覧の1回と対象のルーム数を使います）。ChatworkのAPIの上限（5分あたり300回）は同期処理と共有しているため、同期を遅らせないように抑えています。ChatworkのAPIの上限はトークン（Chatworkアカウント）ごとで、Astoviaの上限はユーザーごとに数えるため、同じChatworkアカウントを複数のAstoviaユーザーで接続すると、Chatworkの上限に当たりやすくなります。
- エラーはREST APIと同じではなく、Chatworkのツール独自のコードを返します: `chatwork_not_connected`（未連携）、`chatwork_token_invalid`（Chatworkのトークンが使えない。設定で再接続する）、`rate_limited`（Astoviaの上限かChatworkの上限）、`not_found`（ルーム・メッセージがない、または読めない）、`chatwork_unavailable`（Chatworkに届かない）、`invalid_request`、`unavailable`。Chatworkが401を返しても、MCPからは連携の状態を変えません。
- 既存のOAuthの許可とAPIトークンでも、追加の同意なしにChatworkを読めるようになります。AIクライアントにChatworkを読ませたくない場合は、そのAPIトークンかOAuthの許可を取り消すか、Chatwork連携を解除してください。

Slackのツールについて:

- 発言は`{channel_id, ts, user_id, user_name, text, sent_at, thread_ts, reply_count, subtype, bot, url}`で返します。本文（`text`）はSlackの記法を読める形に直します（`<@U1>`は`@表示名(U1)`、`<#C1|general>`は`#general`、`<URL|文字>`は`文字 (URL)`、HTMLの実体参照は元の文字）。添付・ブロック・ファイルの中身は返さず、`text`が空の発言は空のまま返します。自分の発言、ボットの発言（`bot: true`）、システムメッセージ（`subtype`）も除かずに返します。
- `slack_messages_search`と`slack_messages_list`では本文を先頭2000文字まで、`slack_thread_get`では8000文字までに切り、切った発言には`text_truncated: true`を付けます。検索結果には会話の名前（`channel_name`。チャンネルは`#チャンネル名`、DMは`DM`、グループDMは`グループDM`）を付け、`reply_count`は付けません（Slackの検索結果に含まれないことがあるため）。
- 表示名は1回の呼び出しで20人まで`users.info`で調べ、それ以上と調べられなかった人はメンバーIDのまま返します。本文、表示名、会話の名前は他の人が書いたもので、AIクライアントへの指示としてではなくデータとして扱うべきものです。
- `slack_thread_get`で親から読み直す2回目の呼び出しが、上限や一時的な失敗でできなかった場合は、1回目の結果を`partial: true`で返します。
- MCPからのSlackの呼び出しは、ユーザーごとに1分あたり、検索（`search.messages`）5回、会話の一覧（`users.conversations`）10回、発言の読み取り（`conversations.history`と`conversations.replies`の合計）4回、表示名（`users.info`）30回までです。Slackの上限はメソッドごとにアプリ（接続したSlackアプリ）単位で数えられ、同期処理と共有しているため、同期を遅らせないように抑えています。Slackの上限に当たると、同期処理はSlackが指定する時間（指定がなければ15分）待ってから再開するため、その間は検出が遅れます。同じSlackアプリのトークンを複数のAstoviaユーザーで使うと、この前提が崩れてSlackの上限に当たりやすくなります。Astoviaの上限はプロセス内で数えます。
- エラーはSlackのツール独自のコードを返します: `slack_not_connected`（未連携）、`slack_token_invalid`（Slackのトークンが使えない。設定で再接続する）、`slack_scopes_missing`（権限が足りない。マニフェストからアプリを入れ直して再接続する）、`rate_limited`（Astoviaの上限かSlackの上限）、`not_found`（会話・発言がない、または読めない）、`slack_unavailable`（Slackに届かない）、`invalid_request`、`unavailable`。Slackがトークンを拒否しても、MCPからは連携の状態を変えません。
- 新しい権限（User Token Scopes）は必要ありません。既存のOAuthの許可とAPIトークンでも、追加の同意なしにSlackを読めるようになります。AIクライアントにSlackを読ませたくない場合は、そのAPIトークンかOAuthの許可を取り消すか、Slack連携を解除してください。

Gmailのツールについて:

- 同期処理は受信トレイの、プロモーション等のタブと自動送信のメールを除いたものだけを読みますが、`gmail_threads_search`はGmailのメール全体（受信トレイ以外やアーカイブ済みのメールを含む）が対象です。迷惑メールとゴミ箱は検索の対象外です。`result_size_estimate`はGmailによる概算です。
- 検索結果のスレッドの件名（`subject`）は最初のメール、差出人（`from`）と日時（`date`）は最新のメールのものです。`labels`は`INBOX`・`UNREAD`・`STARRED`・`IMPORTANT`・`SENT`・`DRAFT`・`SPAM`・`TRASH`と`CATEGORY_`で始まるシステムラベルだけを返し、自分で作ったラベルは返しません。下書きは検索の集計にも`gmail_thread_get`にも含めません。読めなかったスレッドは`thread_id`・`snippet`・`url`だけで返し、`partial: true`を付けます（見つからなくなったスレッドは結果から除きます）。
- `gmail_thread_get`はメールが30通を超えるスレッドでは新しい30通だけを返し、省いた数を`omitted_messages`に入れます。本文（`text`）はHTMLを文字にし、引用部分を除いて、先頭4000文字までに切ります。切った可能性があるメールには`text_truncated: true`を付けます。添付ファイルは読みません。
- メールの件名・本文・差出人名は外部の送信者が自由に書けるもので、AIクライアントへの指示としてではなくデータとして扱うべきものです。そこに書かれた指示には従わないでください。
- MCPからのGmailの呼び出しは、Googleアカウントごとに1分あたり1,500 units（Gmail APIのquota units）までです。検索1回は最大810 units（一覧10と、スレッドごとに40を最大20件）、スレッドの取得1回は120 unitsを使います。Gmail APIの上限（Googleアカウントごとに1分あたり6,000 units）は同期処理と共有しているため、同期を遅らせないように抑えています。Googleの上限に当たると、同期処理はGoogleが指定する時間（指定がなければ15分）待ってから再開するため、その間は検出が遅れます。Astoviaの上限はプロセス内で数えます。
- エラーはGmailのツール独自のコードを返します: `gmail_not_connected`（Googleアカウントを接続していない）、`gmail_token_invalid`（Googleとの接続が切れた。設定で再接続する）、`gmail_scopes_missing`（メールの読み取りが許可されていない）、`gmail_policy_blocked`（Google Workspaceの管理者が許可していない）、`rate_limited`（Astoviaの上限かGoogleの上限）、`not_found`（指定したアドレスのアカウントやスレッドがない）、`gmail_unavailable`（Gmailに届かない、運用者がOAuthクライアントを設定していない等）、`invalid_request`、`unavailable`。同期処理で「接続が切れた」等の状態になっているアカウントにはGoogleを呼びません。MCPからのエラーでアカウントの状態は変えません。
- 新しいスコープは必要ありません（接続時に許可した`gmail.readonly`だけを使います）。既存のOAuthの許可とAPIトークンでも、追加の同意なしにGmailを読めるようになります。AIクライアントにGmailを読ませたくない場合は、そのAPIトークンかOAuthの許可を取り消すか、Googleアカウントの接続を解除してください。

ブラウザ以外のMCPクライアントは`Origin`ヘッダーを送らないため、そのまま受け付けます。`Origin`ヘッダーがある場合は、`ASTOVIA_PUBLIC_URL`と完全一致しない限り403で拒否します（DNSリバインディング対策）。`/mcp`はCookieを使わないため、トークンを持たないWebページからは認証を通りません。

## OAuth

Astoviaは`/mcp`のための認可サーバーを内蔵しています。OAuth 2.1の認可コード方式（PKCE S256必須）だけに対応し、OpenID Connect、トークンの失効・イントロスペクションのエンドポイント、JWT形式のトークンは提供しません。

| エンドポイント | 内容 |
| --- | --- |
| `GET /.well-known/oauth-protected-resource/mcp` | 保護リソースメタデータ。`resource`は`<ASTOVIA_PUBLIC_URL>/mcp`、`authorization_servers`は`ASTOVIA_PUBLIC_URL` |
| `GET /.well-known/oauth-protected-resource` | 上と同じ内容。`/mcp`が唯一の保護リソースなので、ルートにフォールバックしたクライアントにも同じリソースを示します（RFC 9728のURL導出とは厳密には一致しません） |
| `GET /.well-known/oauth-authorization-server` | 認可サーバーメタデータ。`issuer`は`ASTOVIA_PUBLIC_URL` |
| `POST /oauth/register` | 動的クライアント登録（DCR） |
| `GET /oauth/authorize` | 認可要求。同意はWeb画面の`/consent`で行います |
| `POST /oauth/token` | `authorization_code`と`refresh_token`グラント |

- スコープは`mcp`（`/mcp`で対応事項を閲覧・作成・更新し、連携中のChatworkとSlackとGmailを読む）と`offline_access`（リフレッシュトークンを発行する）の2つです。`mcp`は必須で、省略すると`mcp`とみなします。
- 有効期間: 同意画面へ渡す認可要求は10分、認可コードは60秒で1回限りです。アクセストークンは、`offline_access`がある場合は1時間、ない場合は7日です（ChatGPTは`offline_access`を要求しないため、毎時の再接続を避けます）。リフレッシュトークンは90日で、使うたびに新しいトークンへ交換（ローテーション）し、そこから90日に延長します。交換済みのリフレッシュトークンは使えません。
- トークン、認可コード、クライアントシークレットはDBにハッシュだけを保存します。発行済みの認可コードをもう一度使うと（PKCEの検証を通った場合）、そのコードから発行したトークンをすべて失効させます。
- クライアントは2通りの方法で識別します。Client ID Metadata Document（CIMD）では、`client_id`がクライアント自身のHTTPSのURLで、Astoviaがその文書を取得して名前とリダイレクトURIを確認します（Claudeが使用）。動的クライアント登録（DCR）では、`/oauth/register`で`dcr_`で始まる`client_id`を発行します。`token_endpoint_auth_method`は`none`／`client_secret_basic`（既定）／`client_secret_post`です。CIMDのクライアントは常に`none`として扱います。
- リダイレクトURIは1クライアント8件まで、各512文字以内で、HTTPSか、ループバック（`localhost`、`127.0.0.1`等、`[::1]`）のHTTPに限ります。カスタムスキーム、フラグメント、認証情報付きのURLは登録できません。ホスト名は小文字のASCII（国際化ドメインはpunycode）かIPアドレスです。認可時はリダイレクトURIを完全一致で照合し、ループバックのHTTPだけはポート番号の違いを無視します（Claude Codeは毎回異なるポートで待ち受けます）。`localhost`と`127.0.0.1`は別のホストとして扱います。トークン要求のリダイレクトURIは認可要求と完全一致が必要です。
- CIMDの取得は`/oauth/authorize`のときだけ行い、`Cache-Control: max-age`に従って（既定1時間、最長24時間）DBにキャッシュします。取得先は名前解決後のIPアドレスで判定し、ループバック、プライベート、リンクローカル、CGNAT等の公開されていないアドレスへは接続しません。タイムアウトは5秒、本文は64KiBまでで、リダイレクトには従いません。プロキシの環境変数は使いません。トークンの発行・更新はDBの行だけを使い、文書を取得し直しません。
- DCRの登録とCIMDの取得は、ログインと同じく接続元IPごとに1分間で10回までです。リバースプロキシ配下では、全員がプロキシのIPとしてまとめて数えられます。CIMDの取得が制限を超えた場合、期限切れのキャッシュがあればそれを使います。
- 同意ページは、クライアント名、CIMDなら発行元のホスト（DCRでは名前を確認していない旨）、許可後の戻り先のホスト、許可する内容、ログイン中のメールアドレスを表示します。同意は管理画面にログインしているブラウザだけが行えます。
- 接続中のAIクライアントは管理画面の「接続中の AI クライアント」に一覧表示し、削除するとそのトークンは即座に使えなくなります。パスワード再設定とユーザー無効化でも、そのユーザーの接続はすべて削除されます。
- OAuthのアクセストークンは`/mcp`だけで使えます。`/api/v1/*`には使えません。
- トークンは発行時の`ASTOVIA_PUBLIC_URL`に結び付きます。`ASTOVIA_PUBLIC_URL`を変更すると、既存の接続は使えなくなり、AIクライアントから接続し直す必要があります。
- 期限切れの認可とトークンの行は、そのユーザーが新しく接続・トークンを交換するときに削除します。クライアント登録（`oauth_clients`）の行は削除しません。DCRの登録とCIMDの取得により、接続元IPごとに1分間で最大10件ずつ増える可能性があります。
- `/oauth/*`の処理内で返すエラーは`{"error": "...", "error_description": "..."}`の形式です。メソッドの誤り（405）と存在しないURL（404）は、他のURLと同じ`{error: {code, message}}`です。

## GitHub連携と通知

GitHubで自分宛てになったレビュー依頼と割り当てを対応事項として取り込み、PR／Issueの状態から完了を確認できたら根拠付きで自動完了にします。レビュー依頼と割り当ての判定はGitHubの通知APIと検索APIに基づく規則だけで行い、AIは使いません。自分へのメンションは、AIの設定がある場合だけ、AIが読んでやることがあるかを判断します（「メンションの検出」と「タスク候補と予定タスク」を参照）。新しく見つけた対応事項、未対応が続いている対応事項、朝のまとめは、Web Push・Slack・メールのうち選んだ送り先に通知します（「通知」を参照）。

### GitHubトークンを登録する

1. 管理画面の「設定」を開き、「GitHub 連携」のリンクからGitHubのトークン作成ページを開きます（[直接開く](https://github.com/settings/tokens/new?scopes=repo,notifications&description=Astovia)）。
2. **classic**のpersonal access tokenを、スコープ`repo`と`notifications`を付けて作成します。必須は`repo`です（PR／Issueの取得に使い、通知APIも`repo`で読めます）。fine-grained tokenには対応していません。
3. 作成したトークンを「GitHub 連携」に貼り付けて「接続する」を押します。

登録時に`GET /user`でトークンとスコープを確かめ、無効なトークン、`repo`のないトークン、fine-grained tokenは保存しません。トークンは`ASTOVIA_JWT_SECRET`から派生した鍵（AES-256-GCM）で暗号化して保存し、画面・APIには末尾4文字だけを表示します。

- 取り込むのは、自分個人へのレビュー依頼（チーム宛ては対象外）と、Issue／PRへの自分の割り当てと、AIの設定がある場合の自分へのメンションです。接続した直後は、検索APIで現在開いているものを取り込みます（このときは通知しません）。以後は通知APIを60秒ごと（GitHubが指定する間隔）に読み、15分ごとに検索で取りこぼしを補います。
- 取り込んだ対応事項は5分ごとにGitHubの状態を確認し、PRのマージ・クローズ、Issueのクローズ、レビューの提出、レビュー依頼の取り消し、割り当ての解除を確認したら自動で完了にします。「自動で処理したもの」画面で理由と根拠のリンクを確認でき、「未完了に戻す」と、その対応事項は以後自動では完了・再オープンされません。自動で完了したものに再びレビュー依頼・割り当てが来ると、未完了に戻して通知します。手動で完了・対象外にしたものは変更しません。
- トークンが無効になると同期を止め、画面に表示します。トークンを更新すると再開します。GitHubの利用上限に達した場合は、解除される時刻まで待ってから自動で再開します。
- 別のGitHubアカウントのトークンに付け替えた場合と、接続を解除した場合、それまでの対応事項は残し、自動処理の対象から外します。
- パスワード再設定ではGitHub連携を削除しません。アカウントを乗っ取られていた可能性がある場合は、設定画面で連携を確認し、必要なら解除してトークンを登録し直してください（GitHub側でもトークンを削除してください）。

### メンションの検出

AIの設定（「AIの設定」を参照）がある場合、GitHubのIssueとPull Requestで自分が`@`メンションされた投稿を見つけ、その投稿と前後の文脈をAIに読ませて、自分がやることがあるかを判断させます。

- AIは、やることがある（タスク）、自分がやるべきか判断しきれない（タスク候補）、やることがない、の3つに分けます。タスクは、AIが書いたタスク文で対応事項にして通知します（「検出したらすぐ通知する」がオンの場合。重要度が高いものはオフでも通知します）。他の人宛ての依頼にccで含まれているだけのものや宛先が曖昧なものはタスク候補にし、通知しません（「タスク候補と予定タスク」を参照）。やることがないと判断したもの（お礼・報告だけ、ccとして名前を挙げただけで何も求められていない、自分が既に返信している等）は、理由付きで「自動で処理したもの」に残します。「未完了に戻す」で戻せます。メンションから作った対応事項は、今は自動では完了しません。
- 投稿の日時は「通知」で設定したタイムゾーン（未設定ならUTC）の時刻と曜日でAIに渡し、「明日」のような相対的な日時は具体的な日付に直させます。
- 対象は、Issue／PRの本文、コメント、レビュー、レビューコメントです。Discussion、Projectsのdraft issue、コミットへのコメント、チーム宛てのメンション（`@org/team`）は対象外です。コードブロック・インラインコード・引用行・HTMLコメントの中の`@`は数えません。自分の投稿も対象外です。
- 対象にするのは、直近72時間以内で、かつGitHub連携の接続とAPIキーの登録の24時間前より後の投稿です（本文は作成時刻、コメントは更新時刻、レビューは提出時刻で判定します。後から本文を編集して足したメンションは対象外です）。1つのIssue／PRで読むのはコメント・レビュー・レビューコメントのそれぞれ新しい200件までで、それより前のメンションと文脈は扱いません。連携を解除して接続し直すと、直前24時間のメンションをもう一度判定して重複する場合があります。
- 同じIssue／PRのメンションから作った未完了のタスクをAIに渡し、同じ用件のタスクが既にあればやることがないと判断させます。同じ用件のまとまり（同じ出来事・案件の流れ）に属するタスクは、AIが付けた話題名の親タスクでまとめます（「タスクの入れ子」を参照）。
- AIに送るのは、リポジトリ名（非公開のものを含む）、Issue／PRのタイトルと本文、メンションした投稿と前の10件・後の3件の投稿の本文、投稿者のGitHubユーザー名と日時、通知設定のタイムゾーン名、同じIssue／PRの未完了のタスク（最大10件。まとめた親タスクは子タスクの内容付き）です。長い本文は途中で切ります。
- AIを呼ぶのは1回の同期で3回までです。残りと、AIに一時的につながらなかったもの（混雑・上限・タイムアウト等）は次の同期で読み直します。2時間より古い投稿でAIにつながらなかった場合と、6時間以上読み直しが続いた場合は、「メンションを確認: …」という対応事項にします。通知APIの3ページを超える通知が一度に届いた場合、読み直す前に範囲から外れたメンションは取りこぼすことがあります。
- APIキーが無効・残高不足・モデルを使えない場合は、その同期のメンションを「メンションを確認: …」にし、以後はAIの設定を直すまでメンションを扱いません。この間のメンションは、キーを直した後も処理されないことがあります。AIの設定が無い間に届いたメンションも、後から処理しません。

### 通知

通知の送り先は、Web Push（登録した端末）、Slack（自分のSlackアプリのIncoming Webhook）、メール（ログインのメールアドレス）の3つです。「設定」の「通知」の「送り先」の表で、通知の種類（タスクの検出、未対応が続く、期限が近い、期限を過ぎた、朝のまとめ）ごとに、どの送り先に送るかを選んで「保存」します。

- 既定では、すべての種類をWeb Pushに送り、Slackとメールには送りません。以前の版から使っている場合も、何も変えなければこれまでどおりWeb Pushだけに届きます。
- 1つの通知を複数の送り先に送る場合、送り先ごとに別々に送り、失敗したときの再送も送り先ごとに行います。
- どの送り先にも送れない通知（選んだ送り先がない、Web Pushの端末もSlackのWebhookもない等）は作りません。
- 送り先を後から増やした場合、今の期間の「未対応が続く」「期限が近い」「期限を過ぎた」の通知は、増やした送り先にも1回送ります。その日の朝のまとめと、既に作ったタスクの検出の通知は、増やした送り先には送りません。
- 「テスト送信」（Slackとメール）と「テスト通知を送る」（Web Push）で、それぞれの送り先に届くか確認できます。テスト通知は送り先を合わせて1分に3回までです。

#### Web Push

- 「設定」の「通知」で「この端末で通知を受け取る」を押すと、ブラウザの通知許可を求め、この端末を登録します。1ユーザーあたり20台まで登録できます。「テスト通知を送る」で届くか確認できます（1分に3回まで）。
- Web PushはHTTPSのページでだけ使えます（`localhost`は例外）。iPhone／iPadはiOS 16.4以降で、共有メニューの「ホーム画面に追加」で追加したAstoviaから登録してください。
- Web画面のURLが`/admin/`から`/`に変わりました。以前の版でホーム画面に追加していた場合は、いったん削除してから追加し直し、追加し直したAstoviaで「この端末で通知を受け取る」をもう一度押してください。ブラウザのまま使っている端末は、登録済みの通知が新しい画面を開くよう自動で切り替わります（「この端末で通知を受け取る」を押し直しても二重には届きません）。
- 通知の内容は、GitHubとSlackとChatworkとメールで新しく見つけた対応事項（「検出したらすぐ通知する」をオフにすると送りません。ただし、AIが重要度を高と判断したタスクは、この設定にかかわらず通知し、タイトルに【重要】を付けます。「タスク候補と予定タスク」を参照）、作成から一定日数（既定3日）を過ぎても未完了の対応事項、毎日決まった時刻（既定8時）の朝のまとめ（未完了の件数と、前回以降に自動で完了にした件数（対応不要と判断したメンションも含みます）と、タスク候補の件数）です。未完了も自動の完了もない日でも、タスク候補があればまとめを送ります。タスク候補そのものは通知しません。時刻と日数とタイムゾーン、タスク候補を自動で対象外にするまでの日数は「通知」で変更できます。サーバーが止まっていて朝のまとめの時刻を過ぎた日は、まとめを送りません。
- 期限のある未完了の対応事項は、期限の24時間前（期限の24時間以内に作ったものを除く）と、期限を過ぎてから完了・対象外にするまで1日ごとに通知します（回数の上限はありません）。期限を過ぎてから作った・取り込んだものは、作成の1日後から通知します。期限まで24時間を切った対応事項には、作成からの日数による通知は送りません。予定タスクとタスク候補には期限の通知を送りません。期限を変更すると、新しい期限で通知します。この版に更新した直後は、期限を過ぎた未完了の対応事項それぞれに1件ずつ通知が届きます。
- VAPIDの鍵は初回に自動で作成し、DB（`instance_secrets`）に暗号化して保存します。VAPIDの連絡先（`sub`）には`ASTOVIA_PUBLIC_URL`がHTTPSならそのURLを、そうでなければ`mailto:webpush@localhost`を使います。後者はローカル開発用で、AppleのPushサービスでは送信に失敗する場合があります。
- 送信先は、名前解決後のアドレスがグローバルなものだけです（プライベートアドレス、ループバック等には送りません）。送信に失敗した通知は最大4回まで再送し、Pushサービスが購読の終了を返した端末は登録を削除します。送信済みの通知は30日後に削除します。

### Slackへの通知

自分で作ったSlackアプリのIncoming Webhookへ通知を送ります。Slack連携（自分のSlackを読み取る機能）とは別のもので、Slack連携のトークンは送信に使いません。Slack連携をしていなくても使えます。

1. [Slackアプリの管理画面](https://api.slack.com/apps)でアプリを作り（Slack連携で作ったアプリでも構いません）、「Incoming Webhooks」を有効にします。
2. 「Add New Webhook to Workspace」で通知を送るチャンネル（自分へのDMでも構いません）を選び、作られたWebhook URLをコピーします。
3. 「設定」の「通知」の「Slack」に貼り付けて「登録」を押し、「送り先」の表でSlackに送る種類を選んで保存します。

- 登録できるのは`https://hooks.slack.com/services/…/…/…`の形のURLだけです。登録のときにはSlackへ送信しません。「テスト送信」で確かめてください。
- URLは`ASTOVIA_JWT_SECRET`から派生した鍵（AES-256-GCM）で暗号化して保存し、画面・APIには表示しません。
- 送るのは、通知のタイトル（太字）と本文と、Astoviaの画面を開くリンクです。
- Slackが「Webhookがない・使えない」（403・404・410）と返した場合と、保存したURLを読み出せない場合は、Slackへの送信を止めて画面に表示します。Webhookを登録し直すと再開します。止まっている間の通知は、Slackには送りません。
- Slackの流量の制限（429）を受けたときは、そのユーザーのSlackへの送信を1分止めてから再開します。作成から24時間を過ぎても送れなかった通知は送りません。

### メールでの通知

運用者がサーバーにメールの送信サービスを設定すると、通知をメールでも送れます。各ユーザーは「送り先」の表でメールに送る種類を選ぶだけで、宛先はログインのメールアドレスに固定です（別のアドレスは指定できません）。設定していないサーバーでは、表のメールの列は選べません。

| 環境変数 | 内容 |
| --- | --- |
| `ASTOVIA_MAIL_PROVIDER` | `cloudflare`（Cloudflare Email Service）か`resend`（Resend） |
| `ASTOVIA_MAIL_API_KEY` | 送信サービスのAPIキー。CloudflareはEmail Serviceで送信できる権限を持つAPIトークン、Resendは`re_`で始まるAPIキー |
| `ASTOVIA_MAIL_FROM` | 送信元のメールアドレス（表示名を付けない形） |
| `ASTOVIA_MAIL_FROM_NAME` | 送信元の表示名（任意。Resendだけで使う） |
| `ASTOVIA_CLOUDFLARE_ACCOUNT_ID` | CloudflareのアカウントID（`cloudflare`のときだけ必須） |

```sh
# Cloudflare Email Service
ASTOVIA_MAIL_PROVIDER=cloudflare
ASTOVIA_MAIL_API_KEY=<APIトークン>
ASTOVIA_MAIL_FROM=notify@example.com
ASTOVIA_CLOUDFLARE_ACCOUNT_ID=0123456789abcdef0123456789abcdef

# Resend
ASTOVIA_MAIL_PROVIDER=resend
ASTOVIA_MAIL_API_KEY=re_xxxxxxxx
ASTOVIA_MAIL_FROM=notify@example.com
ASTOVIA_MAIL_FROM_NAME=Astovia
```

- 送信元のドメインは、使うサービスで事前に認証（DNSの設定）しておいてください。認証していないドメインからは送れません。
- 送るのは、通知のタイトルを件名に、通知の本文とAstoviaの画面を開くリンクを本文にしたテキストのメールです。末尾に、送り先を変える画面のURLを書きます。HTMLメールと配信停止のリンクはありません。
- Cloudflare Email ServiceとResendの違い: 表示名（`ASTOVIA_MAIL_FROM_NAME`）を送るのはResendだけです。重複を防ぐ冪等キー（通知ごとに一意）を送るのもResendだけです。Cloudflareでは、送信が実際には届いていたのに失敗として再送した場合、同じ通知が2通届くことがあります。
- 送信サービスの流量の制限（429。Resendでは1日・1か月の上限も含みます）を受けたときは、メールの送信を全体で1分止めてから再開します。作成から24時間を過ぎても送れなかった通知は送りません。そのほかの失敗は、Web Pushと同じく最大4回まで再送します。
- 設定の誤り（`ASTOVIA_MAIL_PROVIDER`が`cloudflare`か`resend`以外、キーや送信元がない・形式が正しくない、Cloudflareでアカウントがない、Resendでアカウントを設定した等）は起動エラーになります。APIキーはログとエラーに出しません。
- 設定を外すと、メールに送る種類を選んでいたユーザーの通知は、メールには送らずに終えます。画面では、選んでいた種類を外すことだけができます。

### 運用上の注意

- バックグラウンド処理（GitHubとSlackとChatworkとメールの同期、通知の判定と送信、タスク候補と過ぎた予定タスクの片付け）は`serve`のプロセス内で動きます。**同じDBに対してAstoviaのプロセスを1つだけ起動してください。**複数のプロセスを起動すると、同じ通知を重複して送る場合があります。
- `ASTOVIA_JWT_SECRET`を変更すると、保存したGitHubとSlackとChatworkのトークン、メール連携のGoogleのトークン、AIのAPIキーとVAPIDの秘密鍵を復号できなくなります。GitHub連携とSlack連携とChatwork連携は「保存したトークンを読み出せません」と表示して止まるため、トークンを登録し直してください。メール連携も同じ表示で止まるため、Googleアカウントを接続し直してください。AIの設定も「保存した API キーを読み出せません」と表示して止まるため、キーを登録し直してください。Slackへの通知も「保存した Webhook を読み出せません」と表示して止まるため、Webhookを登録し直してください。VAPIDの鍵は自動で作り直し、すべての端末の登録を削除するため、各端末で「この端末で通知を受け取る」をもう一度押してください。

## Slack連携

Slackで自分へのメンション、自分宛てのDM、（設定すれば）参加中のグループDMを見つけ、AIに読ませて自分がやることがあるかを判断させます。やることがあるものは対応事項に、自分宛てか判断しきれないものはタスク候補にし（「タスク候補と予定タスク」を参照）、その後に自分がその会話で返信したら、返信で用件が済んだかをAIに判断させて、済んでいれば自動で完了にします。**AIの設定（「AIの設定」を参照）がないと、検出も自動完了も行いません。**Slackへの書き込み（投稿・既読・リアクション）はしません。連携したSlackは、MCP（「MCP」を参照）からも読めます。

### Slackアプリを作って接続する

自分のワークスペースに、自分用のSlackアプリを作ります。1ユーザーにつき1ワークスペースです。

1. 管理画面の「設定」→「連携」の「Slack 連携」で、「Slack アプリを作る」のリンク（[直接開く](https://api.slack.com/apps?new_app=1)）を開き、「From a manifest」を選んでワークスペースを選びます。
2. 画面に表示されるマニフェスト（[`deploy/slack-app-manifest.yaml`](deploy/slack-app-manifest.yaml)と同じ内容）を貼り付けて、アプリを作ります。
3. アプリをワークスペースにインストールし、「OAuth & Permissions」の**User OAuth Token**（`xoxp-`で始まる）を「Slack 連携」に貼り付けて「接続する」を押します。Bot Token（`xoxb-`）には対応していません。

登録時に`auth.test`でトークンと権限を確かめ、無効なトークンや権限が足りないトークンは保存しません。トークンは`ASTOVIA_JWT_SECRET`から派生した鍵（AES-256-GCM）で暗号化して保存し、画面・APIには末尾4文字だけを表示します。Slackへのリクエストは`https://slack.com/api`だけに送り、トークンは`Authorization`ヘッダーにだけ載せます。

必要な権限（User Token Scopes）と用途は次のとおりです。

| スコープ | 用途 |
| --- | --- |
| `search:read` | 自分へのメンションと自分宛てのDMを検索する |
| `users:read` | 投稿者の表示名を調べる |
| `channels:read` `groups:read` `im:read` `mpim:read` | 会話の情報と、参加中のグループDMの一覧を読む |
| `channels:history` `groups:history` `im:history` `mpim:history` | メンションの前後の文脈、DM・グループDMの発言、返信を読む |

### 検出のしくみ

- 60秒ごとに同期します。メンションは`<@自分>`を含む発言を検索で、DMは自分宛てのDMを検索で見つけ、その会話（チャンネルかスレッド）の発言を読みます。グループDMは参加中のグループDMを会話IDの順に**20件まで**読みます（21件目以降は読みません。その中のメンションはメンションとして扱います）。
- 検出対象は「設定」で個別に切り替えられます。チャンネルでのメンションとDMは既定でオン、グループDMは既定でオフです。DM／グループDMをオフにすると、その中のメンションはメンションとして扱います。
- DMとグループDMは、自分の発言で区切った相手の発言のかたまり（最大10件）ごとに判断します。判断済みの発言の後に届いた発言は、新しいかたまりとして判断します。メンションは発言ごとに判断し、前の10件と後の3件（スレッドならそのスレッド）を文脈として渡します。
- 自分の発言、ボットの発言、参加・退出などのシステムメッセージは対象外です。
- やることがあると判断したものは、AIが書いたタスク文で対応事項にして通知します（「検出したらすぐ通知する」がオンの場合。重要度が高いものはオフでも通知します）。他の人宛ての依頼にccで含まれているだけのもの、共有された情報に含まれる対応事項、宛先が曖昧な依頼はタスク候補にし、通知しません。やることがないと判断したものは、理由付きで「自動で処理したもの」に残します。接続した直後の取り込みと、2時間より古い発言は通知しません。タスクの参照には、元の発言へのリンクと会話の名前（チャンネルは`#チャンネル名`、DMは相手の表示名、グループDMは「グループDM」）を付けます。
- 発言の日時は「通知」で設定したタイムゾーン（未設定ならUTC）の時刻と曜日でAIに渡し、「明日」のような相対的な日時は具体的な日付に直させます。
- 同じ会話の未完了のタスクと同じ用件ならやることがないと判断させます。このときタスク候補は含めません（候補に後から確かな依頼が吸い込まれないようにするためです）。同じ用件でタスクと候補が両方できることがあります。同じ会話は、同じスレッド（スレッドの最初の発言へのメンションと、その返信へのメンションを含みます）か、スレッドでないDM・グループDMでは同じ相手との直近72時間のやりとりです。
- 同じ会話で、同じ用件のまとまり（同じ出来事・案件の流れ）に属するタスクは、AIが付けた話題名の親タスクでまとめます（「タスクの入れ子」を参照）。
- 対象にするのは、直近72時間以内で、かつSlack連携の接続とAPIキーの登録の24時間前より後の発言です。
- AIを呼ぶのは1回の同期で3回までです。残りと、AIに一時的につながらなかったものは次の同期で読み直します。2時間より古い発言でAIにつながらなかった場合と、6時間以上読み直しが続いた場合は、「Slack を確認: …」という対応事項にします。
- AIに送るのは、ワークスペース名、チャンネル名、判断する発言と前後の発言の本文、投稿者の表示名とメンバーID、日時、通知設定のタイムゾーン名、同じ会話の未完了のタスク（最大10件。まとめた親タスクは子タスクの内容付き）です。

### 返信による自動完了

- 5分ごとに、追跡中の対応事項とタスク候補（最大20件）について、元の発言の後に自分が同じ会話（スレッドならそのスレッド、そうでなければチャンネルとその発言のスレッド）で発言したかを確かめます。発言していれば、その返信で用件が済んだか（回答した、依頼に応じた、不要と伝えた等）をAIに判断させ、済んでいれば「返信で対応済み」として自動で完了にします。「確認します」のような受け止めだけでは完了にしません。AIを呼ぶのは1回の確認で3回までです。タスク候補も同じく完了にします。予定タスクは、返信してもカレンダーに登録したことにはならないため追跡しません。
- 元の発言が消えた場合や会話が見えなくなった場合は、その対応事項を追跡の対象から外します（状態は変えません）。

### 取りこぼしと制約

- Slackの検索は反映が遅れることがあり、10分の重なりを持たせて読み直しても、検索に出るのが遅れた発言は取りこぼすことがあります。1回の検索で読むのは200件まで、1つの会話で読むのは200件までです。
- Slackの呼び出し回数は1回の同期ごとに上限があり（検索4回、会話の読み取り40回など）、超えた分は次の同期で読み直します。6時間以上読み直しが続いた場合は先へ進み、読めなかった会話は取りこぼします。
- Slackが会話の読み取りを拒否した場合（チャンネルが見つからない、参加していない等）、その会話は飛ばします。
- トークンが無効になった場合と権限が足りなくなった場合は同期を止め、画面に表示します。トークンを更新すると再開します。Slackの利用上限（HTTP 429）に達した場合は、指定された時間だけ待ってから再開します。
- 別のワークスペース・別のユーザーのトークンに付け替えた場合と、接続を解除した場合、それまでの対応事項は残し、自動処理の対象から外します。解除して同じワークスペースに接続し直すと、直前24時間の発言をもう一度判断して、重複した対応事項ができることがあります（GitHub連携と同じ制約です）。
- パスワード再設定ではSlack連携を削除しません。アカウントを乗っ取られていた可能性がある場合は、設定画面で連携を確認し、必要なら解除してSlack側でアプリのトークンを無効にしてください。

## Chatwork連携

Chatworkで自分宛てのTO・返信・TO ALL（グループチャット）とダイレクトチャットを見つけ、AIに読ませて自分がやることがあるかを判断させます。判断のしかた（タスク・タスク候補・対応不要、予定タスク、同じ用件のまとめ）はSlack連携と同じです。その後に自分が同じルームで発言したら、その発言で用件が済んだかをAIに判断させて、済んでいれば自動で完了にします。あわせて、自分に割り当てられたChatworkのタスクを（AIを使わずに）そのまま対応事項として取り込みます。**AIの設定（「AIの設定」を参照）がないと、TOとダイレクトチャットの検出と自動完了は行いません（タスクの取り込みは行います）。**Chatworkへの書き込み（投稿・既読・タスクの完了）はしません。

### APIトークンを発行して接続する

1. Chatworkの右上のメニューから「サービス連携」を開き、「APIトークン」でトークンを発行します。組織によっては、管理者によるAPIの利用の許可が必要です。
2. 管理画面の「設定」→「連携」の「Chatwork 連携」に、発行したトークンを貼り付けて「接続する」を押します。

- APIトークンはアカウントに1つです。再発行すると、同じトークンを使っている他のツールも使えなくなります。
- 登録時に`GET /me`でトークンを確かめ、無効なトークンは保存しません。トークンは`ASTOVIA_JWT_SECRET`から派生した鍵（AES-256-GCM）で暗号化して保存し、画面・APIには末尾4文字だけを表示します。Chatworkへのリクエストは`https://api.chatwork.com/v2`だけに送り、トークンは`x-chatworktoken`ヘッダーにだけ載せます（リダイレクトは追いません）。OAuthには対応していません。

### 検出のしくみ

- 60秒ごとに同期します。`GET /rooms`で参加中のルームを読み、前回の同期以降に更新のあったルームだけを、更新の新しい順に**1回に10ルームまで**読みます。各ルームで読むのは最新の100件です（`force=1`で読むため、他のツールが同じトークンで読んでも取りこぼしません）。
- グループチャットでは、引用（`[qt]…[/qt]`）の外に自分宛てのTO（`[To:自分]`）、自分への返信（`[rp aid=自分 …]`）、TO ALL（`[toall]`）を含む他の人の発言を、1件ずつ判断します。前の10件と後の3件を文脈として渡します。TO ALLは全員への連絡や周知だけなら対応不要と判断させます。
- ダイレクトチャットは、自分の発言で区切った相手の発言のかたまり（最大10件）ごとに判断します。判断済みの発言の後に届いた発言は、新しいかたまりとして判断します。「ダイレクトチャットを検出する」をオフにすると、ダイレクトチャットの中のTOと返信だけをグループチャットと同じように判断します。
- 自分の発言、Chatworkのシステムメッセージ（参加・退出やタスクの追加の通知など）は対象外です。タスクの取り込みがオンのときは、タスクを追加した発言（`[task …]`を含む発言）も対象外にし、タスクの取り込みに任せます。
- AIに渡す本文は、`[To:…]`を`@名前(ID)`に、返信を`返信 @名前(ID)`に、`[toall]`を`@all`に、引用を「引用（名前）: …」に直したものです。名前は同じルームの最新100件の発言者と自分の名前から引き、わからない人はIDのままにします。
- やることがあると判断したものは、AIが書いたタスク文で対応事項にして通知します（「検出したらすぐ通知する」がオンの場合。重要度が高いものはオフでも通知します）。接続より前の発言、AIの設定より前の発言、2時間より古い発言は通知しません。タスクの参照には、元の発言へのリンクとルームの名前（ダイレクトチャットは「DM（相手の名前）」）を付けます。
- 同じルームの未完了のタスク（直近72時間）と同じ用件ならやることがないと判断させ、同じ用件のまとまりに属するタスクは、AIが付けた話題名の親タスクでまとめます（「タスクの入れ子」を参照）。
- 対象にするのは、直近72時間以内で、かつChatwork連携の接続とAPIキーの登録の24時間前より後の発言です。AIを呼ぶのは1回の同期で3回までで、残りと、AIに一時的につながらなかったものは次の同期で読み直します。2時間より古い発言でAIにつながらなかった場合と、6時間以上読み直しが続いた場合は、「Chatwork を確認: …」という対応事項にします。
- AIに送るのは、組織名、ルーム名、判断する発言と前後の発言の本文、投稿者の名前とアカウントID、日時、通知設定のタイムゾーン名、同じルームの未完了のタスク（最大10件）です。

### タスクの取り込み

- 5分ごとに`GET /my/tasks?status=open`で自分に割り当てられた未完了のタスク（最大100件）を読み、まだ取り込んでいないものを対応事項にします。AIは使わないため、AIの設定がなくても動きます。本文を1行にしたものをタスク文にし、期限は時刻付きならその時刻、日付だけならその日（「通知」のタイムゾーン）の23:59にします。接続して最初の取り込みは通知せず、2回目以降は「検出したらすぐ通知する」に従って通知します。
- 取り込んだタスクが一覧から外れたら（Chatwork側で完了・削除された、担当から外れた、ルームから退出した等。理由は区別しません）、「Chatwork のタスク一覧から外れた」として自動で完了にします。ただし、一覧がちょうど100件のときと、読めないタスクが混じっていたときは、一覧が欠けている可能性があるため自動で完了にしません。
- Astoviaで完了にしても、Chatworkのタスクは変わりません。
- 「自分に割り当てられたタスクを取り込む」をオフにしても、取り込み済みのタスクの対応事項は残ります。オフの間は一覧から外れたかの確認もしないため、自動で完了にもなりません。

### 発言による自動完了

- 5分ごとに、TOとダイレクトチャットから作った追跡中の対応事項とタスク候補（最大20件）について、元の発言の後に自分が同じルームで発言したかを、そのルームの最新100件から確かめます。発言していれば、その発言で用件が済んだかをAIに判断させ、済んでいれば「返信で対応済み」として自動で完了にします。AIを呼ぶのは1回の確認で3回までです。予定タスクと取り込んだChatworkのタスクは追跡しません。
- 活発なルームで元の発言が最新100件より前に流れた場合も、追跡は続け、その後の発言だけで判断します。ルームが読めなくなった場合（退出した等）は、その対応事項を追跡の対象から外します（状態は変えません）。

### 取りこぼしと制約

- 1つのルームで、前回の同期から次の同期までの間に100件を超える発言があると、古い分は読めません。
- 1回の同期で読むルームは10までです。更新のあるルームが1分に10を超える状態が続くと、更新の古いルームは後回しになり、最大6時間読まれず、その後は読まれないまま先へ進みます（取りこぼします）。
- 編集されたメッセージは読み直しません。
- Chatworkの呼び出しは1回の同期でルーム一覧1回、メッセージ20回（検出10回・自動完了の確認10回）、タスク一覧1回までです（1分に最大22回、5分で110回）。Chatworkの利用上限（HTTP 429）に達した場合は、`x-ratelimit-reset`の時刻（わからなければ15分後）まで待ってから再開します。
- 特定のルームが読めない場合（権限がない、見つからない、応答が大きすぎる）、そのルームは飛ばします。トークンが無効になった場合は同期を止め、画面に表示します。トークンを更新すると再開します。
- 組織によっては、管理者がAPIの利用を許可していないと使えません。
- 別のアカウントのトークンに付け替えた場合と、接続を解除した場合、それまでの対応事項は残し、自動処理の対象から外します。解除して同じアカウントに接続し直しても、同じ発言とタスクの対応事項は重複して作りません。
- パスワード再設定ではChatwork連携を削除しません。アカウントを乗っ取られていた可能性がある場合は、設定画面で連携を確認し、必要なら解除してChatwork側でAPIトークンを再発行してください。

## メール連携（Gmail・Google Workspace）

Googleアカウント（gmail.comでもGoogle Workspaceでも）の受信トレイに届いた他の人からのメールを、スレッドごとにAIに読ませて自分がやることがあるかを判断させます。判断のしかた（タスク・タスク候補・対応不要、予定タスク、同じ用件のまとめ）はSlack連携と同じです。その後に自分が同じスレッドでメールを送ったら、そのメールで用件が済んだかをAIに判断させて、済んでいれば自動で完了にします。1人のユーザーが10個までのGoogleアカウントを接続できます。**AIの設定（「AIの設定」を参照）がないと、何も読みません。**Gmailへの書き込み（既読・ラベル・送信）はしません。接続したGoogleアカウントのGmailは、MCP（「MCP」を参照）からも読めます。

### Google Cloudで準備する（運用者）

メール連携を使うには、Astoviaの運用者がGoogle CloudでOAuthクライアントを作り、そのIDとシークレットを環境変数に設定します。未設定のときは、画面に「運用者の設定が必要」と表示し、接続できません。

1. [Google Cloud コンソール](https://console.cloud.google.com/)でプロジェクトを作ります。
2. 「APIとサービス」で Gmail API を有効にします。
3. OAuth 同意画面を作ります。Google Workspace の組織内のアカウントだけで使うなら「内部」、gmail.com のアカウントも使うなら「外部」を選びます。
4. スコープに`https://www.googleapis.com/auth/gmail.readonly`を足します。Astoviaが求めるのはこのスコープだけです。
5. 「外部」の場合は、公開ステータスを「本番環境」にします。「テスト中」のままだと、7日で接続が切れます。
6. 「認証情報」で「ウェブアプリケーション」の OAuth クライアントを作ります。承認済みのリダイレクト URI は`{ASTOVIA_PUBLIC_URL}/integrations/google/callback`です（画面の「メール連携」にも表示します）。
7. クライアント ID を`ASTOVIA_GOOGLE_CLIENT_ID`に、クライアントシークレットを`ASTOVIA_GOOGLE_CLIENT_SECRET`に設定して、Astoviaを再起動します。2つは両方とも設定してください（片方だけだと起動しません）。

- 「外部」で審査を受けていない場合、接続の途中で「このアプリは Google で確認されていません」と表示されます。運用者が自分で作ったクライアントなので、「詳細」から進んでください。審査を受けていないクライアントは、接続できるGoogleアカウントが合計100までです。「内部」なら警告も上限もありません。
- Google Workspace の管理者が未確認のサードパーティ製アプリを止めている場合は、管理コンソールの「セキュリティ」→「アクセスとデータ管理」→「API の制御」→「アプリのアクセス制御」で、このクライアント ID を「信頼できる」に設定してもらってください。許可されていないと、画面に「Google Workspace の管理者がこのアプリを許可していません」と表示します。

### Googleアカウントを接続する

1. 管理画面の「設定」→「連携」の「メール連携（Gmail・Google Workspace）」で「Google アカウントを追加」を押します。
2. Googleの画面でアカウントを選び、メールの読み取りを許可します。
3. Astoviaの画面に戻り、「（アドレス）を接続しました」と表示されれば完了です。

- 認可はPKCE付きの認可コードで行い、手続きは10分で無効になります。手続きはAstoviaのユーザーに結び付くため、他の人が始めた手続きを自分のブラウザで完了させることはできません。
- 受け取ったリフレッシュトークンは`ASTOVIA_JWT_SECRET`から派生した鍵（AES-256-GCM）で暗号化して保存し、画面・APIには出しません。Googleへのリクエストは`accounts.google.com`、`oauth2.googleapis.com`、`gmail.googleapis.com`だけに送り、リダイレクトは追いません。コールバックのURL（`?code=…&state=…`）は画面を開いた直後にアドレスバーから消し、Astoviaはリクエストの URL をログに出しません。
- 接続が切れた（「Google との接続が切れました」）ときは、そのアカウントの「再接続」を押します。Googleの画面にはそのアドレスが選ばれた状態で出ます。同じアドレスを接続し直すと、それまでの対応事項と検出の進み具合をそのまま引き継ぎます。
- 接続を解除すると、Googleでトークンを失効させます（失敗しても解除は進めます）。それまでの対応事項は残し、自動処理の対象から外します。接続し直しても、同じメールの対応事項は重複して作りません。

### 検出のしくみ

- 60秒ごとに、接続したアカウントごとに同期します。受信トレイ（`in:inbox`）のうち、プロモーション・ソーシャル・フォーラム・新着（更新）のタブとチャットを除いたメールを、前回の同期以降の分だけ一覧で読みます。一覧に出たメールのスレッドを読み、メールの受信日時の順にまとめて判断します。
- 判断するのは、受信トレイにある他の人からのメールです。自分が送ったメール、下書き、ゴミ箱と迷惑メールのメールは判断しません（下書き・ゴミ箱・迷惑メールはAIにも渡しません）。`Auto-Submitted`ヘッダー（`no`以外）、`Precedence: bulk`／`junk`、差出人が`noreply`・`no-reply`・`donotreply`・`do-not-reply`・`mailer-daemon`のメールは自動送信として判断しません。
- 1つのスレッドでは、自分が送ったメールで区切った他の人のメールのかたまり（最大10通）ごとに判断します。前の10通と後の3通を文脈として渡します。判断済みのメールの後に届いたメールは、新しいかたまりとして判断します。
- AIに渡す各メールの本文の先頭には、Astoviaが件名・自分の位置（To／CC／なし／送信者（自分））・宛先・CCの4行を付けます。自分の位置は、宛先とCCに接続したアドレスがあるかで決めます（大文字・小文字と`+`以降を無視し、gmail.com では`.`も無視します）。BCC・メーリングリスト・グループのアドレス・Workspace の別名で届いたメールは「なし」になり、本文で名指しされている依頼でなければタスク候補か対応不要と判断させます。
- 本文はHTMLのパートがあればそれを文字にしたもの（利用者が見ている内容）を、なければテキストのパートを使います。添付ファイルは読みません。引用（`>`で始まる行、「On … wrote:」「…さんは書きました:」以降、Gmailの引用部分）は除きます。
- やることがあると判断したものは、AIが書いたタスク文で対応事項にして通知します（アカウントごとの「検出したらすぐ通知する」がオンの場合。重要度が高いものはオフでも通知します）。接続より前のメール、AIの設定より前のメール、2時間より古いメールは通知しません。タスクの参照には、Gmailのスレッドへのリンクと件名を付けます。
- 同じスレッドの未完了のタスクと同じ用件ならやることがないと判断させ、同じ用件のまとまりに属するタスクは、AIが付けた話題名の親タスクでまとめます（「タスクの入れ子」を参照）。
- 対象にするのは、直近72時間以内で、かつ接続とAPIキーの登録の24時間前より後に受信したメールです。AIを呼ぶのは1回の同期で3回までで、残りと、AIに一時的につながらなかったものは次の同期で読み直します。2時間より古いメールでAIにつながらなかった場合と、6時間以上読み直しが続いた場合は、「メールを確認: …」という対応事項にします。

### 返信による自動完了

- 5分ごとに、メールから作った追跡中の対応事項とタスク候補（アカウントごとに最大20件）について、元のメールの後に自分が同じスレッドでメールを送ったかを確かめます。送っていれば、そのメールで用件が済んだかをAIに判断させ、済んでいれば「返信で対応済み」として自動で完了にします。下書きは送ったメールとみなしません。AIを呼ぶのは1回の確認で3回までです。予定タスクは追跡しません。
- スレッドが読めなくなった場合（削除された等）は、その対応事項を追跡の対象から外します（状態は変えません）。

### 取りこぼしと制約

- 1回の同期で読む一覧は、通常は300通（3ページ）、接続直後や長く止まった後は1,000通（10ページ）までです。それを超える古い分は読まずに先へ進みます（取りこぼします）。1回の同期で読むスレッドにも上限（検出12・自動完了の確認8）があり、残りは次の同期に回します。
- 受信日時が72時間より古いメールは読みません（迷惑メールやアーカイブから受信トレイに戻したメール、POPで取り込んだメールも含みます）。
- AIに送る本文は、判断するメールは2,000文字、文脈のメールは800文字までです。
- HTMLメールの隠し文字は、`style`属性（`display: none`、`visibility: hidden`、大きさ・不透明度が0など）と`hidden`属性・`aria-hidden="true"`で隠したものだけを除きます。`<style>`のクラス指定、画面外への配置、文字色と背景色を同じにする等で隠した文字は除けません。
- Gmailへは書き込みません（既読・ラベル・送信はしません）。
- Workspace の別名やグループのアドレス宛てのメールは「自分の位置: なし」として判断します（本文で名指しされていればタスクになりえます）。
- 1つのGoogleアカウントは、1人のAstoviaユーザーにしか接続できません（Googleでのトークンの失効が、そのアカウントの許可全体を取り消すためです）。
- Google側でアドレスが変わった（Workspaceの改名など）ときは、古いアドレスの接続を先に解除してから、新しいアドレスで接続し直してください。同じGoogleアカウントを2つのアドレスで接続したまま片方を解除すると、両方が失効します。
- 接続に失敗したときに受け取ったトークンは保存せずに捨て、失効もさせません（同じGoogleアカウントの他の接続まで失効させないためです）。Google側に残った許可は、Googleアカウントの「サードパーティ製のアプリとサービス」から削除できます。
- 同じアカウントの解除と再接続を並行して行うと、再接続で保存したトークンが解除の失効で使えなくなることがあります。その場合は同期で「接続が切れました」と表示されるので、もう一度接続してください。
- ユーザーを無効化したときは、接続したアカウントを削除しますが、Google側の許可は取り消しません。必要ならGoogleアカウントの「サードパーティ製のアプリとサービス」から削除してください。
- AIを設定していない間は同期しない（Googleにも問い合わせない）ため、接続が切れても画面には「正常」と表示されたままです。
- Googleの利用上限（HTTP 429、利用上限の403）に達した場合は、指定された時間（わからなければ15分後）まで待ってから再開します。トークンが失効した場合、メールの読み取りの許可が外された場合、Workspaceの管理者がアプリを止めた場合は同期を止め、画面に表示します。Gmail API が無効な場合と、OAuthクライアントの設定が正しくない場合は、運用者が直すまで間隔を空けて試し続けます。
- パスワード再設定ではメール連携を削除しません（途中の接続の手続きは取り消します）。アカウントを乗っ取られていた可能性がある場合は、設定画面で接続しているアカウントを確認し、必要なら解除してください。

## タスク候補と予定タスク

GitHubのメンションとSlackの発言から、AIは「タスク」「タスク候補」「対応不要」の3つに分けて対応事項を作ります。

- タスク候補は、自分がやるべきかAIが確信を持てないもの（ccとして挙げられた投稿に含まれる依頼、共有された情報に含まれる対応事項、宛先が曖昧な依頼）です。通知はせず、メニューの「タスク候補」の画面に並べます（メニューに件数を表示します）。タスク画面の「未完了」の先頭には「タスク候補が N件 あります」と1行で知らせ、開くと新しいもの3件までをその場で確認できます。「タスクにする」で未完了のタスクに、「不要」で対象外にします。Slackの候補は、自分の返信で用件が済んだとAIが判断すれば自動で完了にします（タスクにした後も同じです）。
- そのまま残ったタスク候補は、最後に変更されてから「通知」の「タスク候補を自動で対象外にするまで」の日数（既定3日。「しない」も選べます）が過ぎると、5分ごとの処理で「候補のまま期間が過ぎた」として対象外にします。
- 自分が参加する会議・打ち合わせなどの日時（開始時刻まで）が決まった投稿は、予定タスク（例「10/29(木) 12:30 ◯◯の打ち合わせを登録する」）にします。期限には予定の開始時刻が入ります。タスク画面の「Google カレンダーに追加」で、件名と日時（終了時刻がなければ1時間）と元の投稿のリンクを入れたGoogleカレンダーの予定作成画面を開きます。登録したら「登録済み」（または完了のチェック）で完了にしてください。招待や他のアプリで既にカレンダーに入っている場合も、「登録済み」で片付けられます。Astovia はカレンダーを読まないため、別の方法で登録された予定には気づきません。GoogleカレンダーのAPIとは連携しません。
- 予定の開始時刻を過ぎても残っている予定タスクは、5分ごとの処理で「予定の時刻を過ぎた」として対象外にします。見つけた時点で開始時刻を過ぎている予定は、最初から対象外として作ります（通知しません）。自動で対象外にしたものは「対象外」の一覧で確認でき、「未完了に戻す」で戻せます。
- 自動で対象外にしたものは、朝のまとめの「自動で処理」の件数と「自動で処理したもの」画面には含めません。
- AIはタスクに重要度（高・中・低）を付けます。重要度が高いものは「検出したらすぐ通知する」がオフでも通知し、通知のタイトルに【重要】を付けます。タスク候補と予定タスク、2時間より古い投稿・発言・メール、接続直後の取り込みは、オフのときは重要度が高くても通知しません。重要度はタスクの詳細画面で変更できます。レビュー依頼・割り当て・Chatworkのタスク、Web画面で作ったタスクと、REST API・MCPで`importance`を省略して作ったタスクは「中」です。
- 見つけた対応事項と未完了が続く対応事項と期限の通知を押すと、そのタスクの詳細画面（`/tasks/{id}`）を開きます。朝のまとめは従来どおりタスクの一覧（自動で処理したものがあれば「自動で処理したもの」）を開きます。この版より前に作られて送信待ちだった通知は、タスクの一覧を開きます。

## タスクの入れ子

タスクは親と子の2段まで入れ子にできます（子の下に孫は作れません）。

- タスクの一覧（未完了・完了・対象外・すべて）とタスク候補の画面には、親を持たないタスクを並べます。子タスクは親タスクの詳細画面で見ます。ただし、親と違う状態の子タスク（例: 親を手で完了・対象外にした後も未完了のまま残った子）は、その状態の一覧に「親: …」と親タスクの内容を付けて並べます（「すべて」には並べません）。「親: …」を押すと親タスクの詳細画面を開きます。
- 一覧の各タスクの本文を押すと、そのタスクの詳細画面（`/tasks/{id}`）を開きます。詳細画面では、補足・期限・予定の日時・作成（検出）日時・元の投稿へのリンク・自動で処理した理由を表示し、状態の変更、子タスクの一覧（作成の古い順、100件まで）と追加、親子の付け替えができます。本文・補足・期限の編集はできません。
- 親タスクの行には、子タスクの進み具合（例「2/3」。対象外にした子は数えません）と、未完了・タスク候補の子タスクのうち一番近い期限（「子の期限 …」）を表示します。「期限切れ・まもなく期限」には、親タスク自身か子タスクの期限が期限切れ・まもなく期限なら親タスクを入れ、近い方の期限の順に並べます。
- 未完了で親を持たないタスクの詳細画面の「子タスクを追加」で、子タスクを作れます。子タスクの詳細画面か親タスクの詳細画面の「親から外す」で親から外し、未完了で親も子も持たないタスクの詳細画面の「親タスクに入れる」で、未完了で親を持たないタスク（新しい順に50件まで）の下に移せます。REST・MCPでは`parent_id`で指定します。子を持つタスクを別のタスクの子にすることはできません。
- 子タスクがすべて片付く（完了か対象外で、1件以上が完了）と、親タスクを「子タスクがすべて完了した」として自動で完了にします。「自動で処理したもの」に残り、「未完了に戻す」で戻せます。自動で完了した親は、子タスクが未完了に戻るか子タスクが増えると、自動で未完了に戻ります。手で完了・対象外にした親は変えず、親を完了・対象外にしても子タスクはそのまま残ります。親の自動完了は通知せず、朝のまとめの「自動で処理」の件数には入ります。
- GitHubとSlackで見つけたタスクは、同じ会話（同じIssue／PR、同じスレッド、スレッドでないDM・グループDMは同じ相手）の未完了のタスクと同じ用件のまとまりだとAIが判断した場合だけ、AIが付けた話題名の親タスク（参照は会話そのもののリンク）でまとめます。同じ会話でも別の用件ならまとめません。会話をまたいではまとめません。タスク候補はまとめません。子タスクがすべて完了して自動で完了した親タスクにも、72時間以内なら同じ用件の新しいタスクを入れて未完了に戻します。
- AIの判断は間違うことがあります。違うまとまりに入ったタスクは「親から外す」か、別の親に入れ直してください。手で親から外したタスクも、次に同じ用件と判断されると再びまとめられることがあります。
- 連携を解除すると、まとめた親タスクも自動処理の対象から外れるため、接続し直した後は同じ会話に新しい親タスクができることがあります。
- 追跡中のタスク（Slackの返信による自動完了の対象）を手で親タスクにした場合、子タスクがすべて片付いて親が自動で完了すると追跡は外れ、未完了に戻しても追跡は再開しません。

## AIの設定

GitHubのメンションとSlack・Chatwork・メールの発言を読んで、自分がやることがあるか（返信で用件が済んだかも）を判断するAIを、ユーザーごとに設定します。提供元は「AnthropicのAPIキー」「ChatGPTのプラン（OpenAI）」「OpenAIのAPIキー」「OpenAI互換の接続先」のどれか1つです。別の提供元に切り替えるときは、今のAIの設定を削除してから設定し直します。

### AnthropicのAPIキー

1. [Anthropic ConsoleのAPIキーのページ](https://platform.claude.com/settings/keys)でAPIキーを作ります。
2. 管理画面の「設定」→「AI」で「Anthropic の API キー」を選び、キーを貼り付け、モデル（Claude Haiku 5.5（既定）、Claude Sonnet 5.5、Claude Opus 5.5）を選んで「保存」を押します。

- 保存時に、そのキーで選んだモデルを使えるかをAnthropicのModels API（トークンを消費しません）で確かめます。キーは`ASTOVIA_JWT_SECRET`から派生した鍵で暗号化して保存し、画面・APIには末尾4文字だけを表示します。`ASTOVIA_JWT_SECRET`を変えるとキーを復号できなくなるため、登録し直してください。サーバーの環境変数（`ANTHROPIC_API_KEY`等）は使いません。
- 費用は登録したキーの組織に請求されます。利用量の集計や上限はありません。

### ChatGPTのプラン（OpenAI）

自分のChatGPTのプラン（Plus・Proなど）の利用枠で判断します。OpenAIの「Sign in with ChatGPT」（オープンソースのクライアント向けのChatGPT plan usage）を使います。OpenAIはサインイン後の戻り先として`http://127.0.0.1:1455/auth/callback`しか認めないため、戻り先のページのURLを手で貼り付けて接続します。

1. 管理画面の「設定」→「AI」で「ChatGPT のプラン」を選び、「ChatGPT で続ける」を押します。新しいタブでChatGPTのサインイン画面が開きます。
2. サインインして許可すると、そのタブに「このサイトにアクセスできません」などと表示されます。それで正常です。
3. そのタブのアドレスバーのURL（`http://127.0.0.1:1455/auth/callback?…`）をすべてコピーし、Astoviaの画面の欄に貼って「接続」を押します。「ChatGPT で続ける」を押してから10分以内に貼ってください。

- 接続時に、OpenAIのIDトークン（署名・発行者・対象・期限・nonce）と、プランを使う許可（`chatgpt.tokens.use.direct`）を確かめます。続けて、プランで使えるモデルの一覧を読み、選んだモデルに小さな確認の要求を1回送り、Astoviaが使う形式（JSON Schemaの構造化出力）で答えられることを確かめてから保存します。新しく接続したときは、一覧にある軽いモデル（名前にLunaを含むもの）を選び、無ければ一覧の最初のモデルを選びます。つなぎ直しでは、それまでのモデルが一覧にあればそのまま使います。選んだモデルで使えなければ、一覧の次のモデルを1回だけ試します。モデルは後から一覧の中で変えられます。
- 判断のたびに、ChatGPTのプランの利用枠を使います。使用量は[ChatGPTの設定](https://chatgpt.com/settings/usage)で確認・管理できます。利用上限に達すると、その旨を表示して15分はAIを呼びません（その間の発言は読み直しを待ち、6時間以上続けば確認用の対応事項にします）。プランで使えない（無料プランなど）場合は接続できません。
- 保存するのは、OpenAIが発行したクライアントID、アカウントの識別子とメールアドレス（画面の表示用）、リフレッシュトークンとアクセストークンで、`ASTOVIA_JWT_SECRET`から派生した鍵で暗号化します。トークンは画面・APIに返しません。リフレッシュトークンは更新のたびに入れ替わり、同じ利用者の更新はAstovia内で1つずつ行います。ChatGPT側で接続を解除した場合などは「ChatGPT との接続が切れました」と表示するため、「つなぎ直す」から接続し直してください。
- AIの設定を削除すると、OpenAIでリフレッシュトークンを取り消します。取り消しに失敗したときは画面に表示するため、ChatGPTの設定からAstoviaの接続を解除してください。つなぎ直し・接続の失敗・パスワード再設定とユーザー無効化では取り消しません（使わなくなったトークンは30日で失効します）。
- **ChatGPTのプランを使う場合、Astoviaのアプリ（プロセス）は1つだけ動かしてください。**トークンの更新を1つずつ行うための排他はプロセスの中だけで行うため、複数のコンテナやプロセスで同じDBを使うと、リフレッシュトークンの再使用として接続が切れることがあります。
- OpenAIのAPIキー（従量課金）を使う場合は、「OpenAIのAPIキー」を選んでください。

### OpenAIのAPIキー

1. [OpenAIのAPIキーのページ](https://platform.openai.com/api-keys)でAPIキーを作ります。
2. 管理画面の「設定」→「AI」で「OpenAI の API キー」を選び、キーとモデル名（例: `gpt-5-mini`）を入力して「保存」を押します。

- OpenAIのChat Completions API（`https://api.openai.com/v1/chat/completions`）を使い、答えはJSON Schemaの構造化出力（`response_format`の`json_schema`、`strict: true`）で受け取ります。モデルはこれに対応している必要があります。推論の強さは`low`を指定し、モデルが推論の強さを受け付けない場合は、指定を外して1回だけ送り直します。
- 保存の前に、そのキーとモデルで小さな確認の要求を1回送り、Astoviaが使う形式で答えられることを確かめます（トークンを少し消費します）。モデル名は一覧から選ぶのではなく、自由に入力します。
- キーは`ASTOVIA_JWT_SECRET`から派生した鍵で暗号化して保存し、画面・APIには末尾4文字だけを表示します。モデルだけを変えるときは、キーの欄を空のままにすれば保存済みのキーを使います。
- 費用は登録したキーの組織に請求されます。利用量の集計や上限はありません。
- 1回の判断は20秒までです。応答の遅いモデルでは判断できず、確認用の対応事項になります。
- Astoviaは保存に関する指定を送りません。送った内容の扱いは提供元・接続先の方針に従います。

### OpenAI互換の接続先

OpenRouter、Ollama、vLLM、LM Studioなど、OpenAIのChat Completions APIと同じ形式の接続先で判断します。

1. 管理画面の「設定」→「AI」で「OpenAI 互換」を選びます。
2. 接続先のURL（`/chat/completions`の手前まで。例: `https://openrouter.ai/api/v1`、`http://ollama:11434/v1`）、必要ならAPIキー、モデル名（例: `meta-llama/Llama-3.1-8B-Instruct`）を入力して「保存」を押します。

- 接続先は`POST {URL}/chat/completions`と、JSON Schemaの構造化出力（`response_format`の`json_schema`、`strict: true`）に対応している必要があります。推論の強さなど、拒否されやすい指定は送りません。APIキーは`Authorization: Bearer`で送り、キーがなければヘッダーを付けません。
- 保存の前に、小さな確認の要求を1回送り、URL・キー・モデル・構造化出力のすべてが通ることを確かめます（接続先によってはトークンを少し消費します）。確認の要求は利用者ごとに1分10回までです。
- URLとキーは`ASTOVIA_JWT_SECRET`から派生した鍵で暗号化して保存します。画面・APIにはURLとキーの末尾4文字（短いキーでは表示しません）を表示し、キー自体は返しません。URLを変えるときは、保存済みのキーを別の接続先へ送らないよう、キーも入れ直してください（キーのない接続先では空のまま）。
- 1回の判断は20秒までです。CPUで動かすローカルモデルなど、応答の遅いモデルでは判断できず、確認用の対応事項になります。
- Astoviaは保存に関する指定を送りません。送った内容の扱いは提供元・接続先の方針に従います。
- URLは`https`だけで、認証情報・クエリ・フラグメントは付けられません。接続するのは公開アドレスだけで、名前解決の後に接続先のアドレスを確かめます。リダイレクトはたどらず、プロキシの環境変数（`HTTPS_PROXY`など）も使いません。証明書はシステムのCAだけで検証し、自己署名の証明書のHTTPSには対応しません（接続できないときと同じ表示になります）。
- 自分のLANやコンテナで動かす接続先を使う場合は、サーバーの環境変数`ASTOVIA_AI_ALLOW_PRIVATE_ENDPOINTS=true`を設定します。`http`と非公開アドレス（ループバック・プライベートアドレスなど）を使えるようになります。既知のクラウドのメタデータのアドレス（169.254.0.0/16、fd00:ec2::254、100.100.100.200）とリンクローカルアドレスは、設定にかかわらず常に拒否します。設定を外すと、保存済みの`http`や非公開アドレスの接続先は使えなくなり、「API キーか接続先の URL が使えなくなりました」と表示して止まります。
- **`ASTOVIA_AI_ALLOW_PRIVATE_ENDPOINTS`のリスク**: 設定すると、ログインできる全員（Cloudflare Accessの方式で自動で作られるメンバーも含む）がURLを指定して、サーバーから内部ネットワークへ要求を送れるようになります。エラーの種類（接続できない・401・404など）から、内部の様子を推測することもできます。上のメタデータのアドレス以外の内部のアドレスは守れません。公開アドレスへの`http`も通るため、APIキーが平文で送られることがあります。信頼できる利用者だけで使う場合に設定してください。

### 共通

- 判断のため、メンションのあったIssue／PRについて、リポジトリ名（非公開のものを含む）、タイトルと本文、コメントの本文、投稿者のGitHubユーザー名と日時が選んだ提供元または接続先に送られます。Slack連携では、判断する発言と前後の発言の本文、投稿者の表示名、ワークスペース名とチャンネル名も送られます（「Slack連携」を参照）。Chatwork連携では、判断する発言と前後の発言の本文、投稿者の名前とアカウントID、組織名とルーム名が送られます（「Chatwork連携」を参照）。メール連携では、判断するメールと前後のメールの本文、件名、差出人の名前とアドレス、宛先とCCの名前とアドレス、接続したGoogleアカウントのアドレスが送られます（「メール連携（Gmail・Google Workspace）」を参照）。ChatGPTのプラン（OpenAI）には、保存しない指定（`store: false`）で送ります。
- AIが書いたタスク文と要約は外部の文章をもとにしています。画面ではテキストとして表示し、MCPでは他の対応事項と同じく`content`として返します。第三者がコメントに「対応不要」などと書くと、AIが対応不要と判断する場合があります。その結果は「自動で処理したもの」に残り、未完了に戻せます。
- パスワード再設定とユーザー無効化では、AIの設定とAIの追加の指示を削除します（乗っ取られていた間に登録されたキーや接続で、本文が他人の組織やアカウントへ送られ続けないようにするためです。追加の指示は、乗っ取られていた間に書かれた指示で検出が黙って止まり続けないようにするためです）。
- `ASTOVIA_JWT_SECRET`を変えると、保存したAPIキーもChatGPTの接続もOpenAI互換の接続先の設定も復号できなくなるため、登録し直してください（ChatGPTの接続に使うホストの識別子も変わります）。

### 追加の指示

管理画面の「設定」→「AI」の「追加の指示」で、AIの判断の基準を自分向けに調整できます（AIの設定がなくても編集・保存できます）。欄は8つです。

- すべての判断: 下の7つすべてに効きます。
- タスクの検出: GitHub（メンション）・Slack・Chatwork・メールの4つ。タスク／タスク候補／対応不要の判断と重要度の基準を調整できます。
- 返信で済んだかの判断: Slack・Chatwork・メールの3つ。どういう返信なら用件が済んだとするかの基準を調整できます。

- 各欄は前後の空白を除いて2000文字までです。空にするとその欄の指示を削除します。
- AIの判定のたびに、固定のシステムプロンプトの末尾に「すべての判断」とその処理の欄の指示を付けて送ります。両方が空なら何も付けません。
- 固定の決まり（出力の形式と項目、元の会話にないことを事実として補わないこと、迷ったときの扱い）は追加の指示より優先します。追加の指示で変えられるのは上の判断の基準だけです。
- 同期の途中で保存した指示は、次の同期から効きます。

#### 試す

「すべての判断」以外の7つの欄には「試す」ボタンがあり、保存する前に指示の効き目を確かめられます（AIの設定が有効なときだけ使えます）。

- 押すと、その連携で最近AIが判定したものを最大10件並べます。タスクの検出では、今の接続の判定から作られたもの（AIが判定できなかったときの確認タスク、まとめ用の親、Chatworkのタスクの取り込み、GitHubのレビュー依頼・割り当ては除く）を新しい順に出します。返信の判断では、返信で済んだとして完了したものと、追跡中で一度でも確認したものを最大5件ずつ出します。
- 1件選ぶと、元の会話を連携先から読み直して判定の入力を作り直し、保存済みの指示と、画面で書きかけの指示（「すべての判断」の欄を含む）で1回ずつ、合わせて2回AIを呼んで、結果を「保存済みの指示」「この指示」の2列で並べます。違う項目は強調します。書きかけの指示は保存しません。
- タスクの作成・更新、通知、AIの設定の状態、追跡の確認時刻は一切変えません。
- 元の判定のときと会話の内容（その後の投稿、関連するタスク）が変わっているため、元の判定と同じ入力にはなりません。関連するタスクからは、試すタスク自身を除きます（除かないと、AIが既存の重複として対応不要と判断するためです）。
- 返信の判断は、元の投稿より後に自分の投稿がないと試せません。
- 試せるのは1ユーザーあたり1分に5回までです。SlackはMCPのSlackの読み取りと同じ回数の枠を使うため、1分に1回程度になります。Chatworkは各ルームの最新100件より古い発言は試せず、MCPのChatworkの読み取りと同じ枠を使います。メールはMCPのGmailの読み取りと同じ枠を使います。

## URLと応答

| URL | 動作 |
| --- | --- |
| `GET /`、`/candidates`、`/auto`、`/settings`、`/settings/<page>`、`/account`、`/account/<page>`、`/tasks/<id>`、`/consent`、`/invite`、`/reset-password` | 埋め込みWeb画面（`index.html`） |
| `GET /assets/*`、`/favicon.ico`等 | 埋め込みWeb画面の静的ファイル（`/sw.js`、`/manifest.webmanifest`を含む）。存在しないファイルは404 |
| `GET /admin/*` | 以前の版のURL。対応する新しいURLへ308でリダイレクト（不明なものは`/`へ）。`/admin/oauth/authorize`は`/consent`へ。`/admin/sw.js`は古い登録の更新用に`/sw.js`と同じ内容を返す |
| `GET /healthz` | プロセスの稼働確認のみ。非公開情報は返さない |
| `POST /auth/login` | 本文は`{ "email": "alice@example.com", "password": "…" }` |
| `POST /auth/refresh` | Cookieのトークンを更新。本文は`{}` |
| `POST /auth/logout` | セッション失効とCookie削除。本文は`{}` |
| `POST /auth/invitations/inspect` | 本文`{token}`で招待の`{email, role, expires_at}`を返す。無効・期限切れ・受諾済み・取り消し済み・存在しない招待は区別せず404`invitation_invalid` |
| `POST /auth/invitations/accept` | 本文`{token, password}`でユーザーを作成し、Loginと同じ応答とCookieを返す。同じメールアドレスのユーザーがいれば409`email_taken`、パスワードが12〜72バイトでなければ400。リクエストに付いてきた既存のRefresh Cookieのセッションは失効させる。作成後のログインだけに失敗した場合は503（ログイン画面からログインできる） |
| `POST /auth/password-resets/inspect` | 本文`{token}`で再設定の`{email}`を返す。無効・期限切れ・使用済み・取り消し済みのリンクと、無効化されたユーザーのリンクは区別せず404`reset_invalid` |
| `POST /auth/password-resets/complete` | 本文`{token, password}`でパスワードを再設定し、Loginと同じ応答とCookieを返す。既存のRefresh Cookieの扱いと503はacceptと同じ |
| `GET /api/v1/me` | 本人の`{id, email, role, password_reset}`。Bearer Access JWTが必要 |
| `GET /api/v1/users` | ユーザーの一覧`{users: [{id, email, role, disabled, created_at}]}`（作成日時の古い順、1000件まで）。オーナーのAccess JWTが必要（メンバーは403`forbidden`、APIトークンは401。以下のユーザー管理APIも同じ） |
| `PATCH /api/v1/users/{id}` | 本文`{role}`か`{disabled}`のどちらか1つで変更し、変更後のユーザーを返す。今と同じ値なら何もせず200。最後の有効なオーナーをメンバーにすると409`last_owner`、自分の無効化は409`self`、存在しないユーザーは404 |
| `POST /api/v1/users/{id}/password-resets` | パスワード再設定リンクを発行し201`{url, expires_at}`を返す（本文なし）。`url`は一度だけ返す。無効化されたユーザーは409`user_disabled` |
| `GET /api/v1/invitations` | 有効な招待の一覧`{invitations: [{id, email, role, created_at, expires_at, invited_by_email}]}`（新しい順、200件まで） |
| `POST /api/v1/invitations` | 本文`{email, role}`で招待を作成し201`{id, email, role, created_at, expires_at, url}`を返す。`url`は一度だけ返す。同じメールアドレスのユーザーがいれば（無効化中も含め）409`email_taken` |
| `DELETE /api/v1/invitations/{id}` | 有効な招待を取り消し204を返す。無い・既に無効な招待は404 |
| `GET /api/v1/action-items` | 本人の対応事項の一覧。クエリは`status`（`open`／`candidate`／`done`／`dismissed`）、`limit`（1〜100、既定50）、`cursor`、`auto`（`true`のみ。自動で処理したもの（自動で完了・対象外にしたもの）だけを処理の新しい順に返す。カーソルは`auto`の有無で別物で、混ぜると400）、`parent_id`（その親の子タスクを作成日時の古い順に100件まで返す。ページングはなく、他のクエリと併用すると400。存在しない・他のユーザーの親は空の一覧）、`view`（`top`のみ。親を持たない対応事項と、親と違う状態の子タスクだけを返す。`status`を省略した場合は親を持たない対応事項だけ。並びとカーソルは`view`なしと同じで、`auto`・`parent_id`と併用すると400）。`{items, next_cursor}`を返す |
| `POST /api/v1/action-items` | 対応事項を作成し201と`Location`を返す。本文は`{content, status?, note?, due_at?, refs?, parent_id?, importance?}` |
| `GET /api/v1/action-items/{id}` | 本人の対応事項を1件返す |
| `PATCH /api/v1/action-items/{id}` | 指定したキー（`content`／`status`／`note`／`due_at`／`refs`／`parent_id`／`importance`）だけを更新する。`note`／`due_at`は`null`で消去、`parent_id`は`null`で親から外す。`version`を付けると一致しない場合に409 |
| `GET /api/v1/api-tokens` | 本人のAPIトークンの一覧`{items: [{id, name, created_at, last_used_at}]}`。トークン本体は含まない。Access JWTが必要 |
| `POST /api/v1/api-tokens` | APIトークンを発行し201を返す。本文は`{name}`（4KiB以内）。応答の`token`は一度だけ返す。50件に達していると409。Access JWTが必要 |
| `DELETE /api/v1/api-tokens/{id}` | APIトークンを削除し204を返す。他のユーザーのトークンと存在しないトークンは404。Access JWTが必要 |
| `GET /api/v1/oauth/consent?request=…` | 同意ページの表示内容`{client_name, client_kind, client_host, redirect_host, redirect_loopback, scopes, expires_at}`。Access JWTが必要。無効・期限切れの要求は400 |
| `POST /api/v1/oauth/consent` | 本文`{request, approve}`（16KiB以内）で許可・拒否し、`{redirect_url}`を返す。Access JWTが必要 |
| `GET /api/v1/oauth/connections` | 接続中のAIクライアントの一覧`{items: [{id, client_name, client_host, scopes, created_at, last_used_at, expires_at}]}`。Access JWTが必要 |
| `DELETE /api/v1/oauth/connections/{id}` | 接続を削除し204を返す。そのトークンは即座に使えなくなる。他のユーザーの接続と存在しない接続は404。Access JWTが必要 |
| `GET /api/v1/integrations/github` | GitHub連携の状態。未接続は`{connected: false}`、接続済みは`{connected, login, token_last4, scopes, immediate_notify, status, last_error, last_synced_at, created_at}`。トークン本体は返さない。Access JWTが必要 |
| `PUT /api/v1/integrations/github` | 本文`{token, immediate_notify?}`（16KiB以内）でトークンを登録・置き換える。無効なトークンは400`github_token_invalid`、`repo`がなければ400`github_scopes_missing`、GitHubに届かなければ503`github_unavailable`。Access JWTが必要 |
| `PATCH /api/v1/integrations/github` | 本文`{immediate_notify}`だけを受け付ける。未接続は404。Access JWTが必要 |
| `DELETE /api/v1/integrations/github` | 連携を解除し204を返す。未接続でも204。Access JWTが必要 |
| `GET /api/v1/integrations/slack` | Slack連携の状態。未接続は`{connected: false}`、接続済みは`{connected, team_name, team_url, user_name, token_last4, scopes, immediate_notify, detect_mentions, detect_dms, detect_group_dms, status, last_error, last_synced_at, created_at}`。トークン本体は返さない。Access JWTが必要 |
| `PUT /api/v1/integrations/slack` | 本文`{token, immediate_notify?, detect_mentions?, detect_dms?, detect_group_dms?}`（16KiB以内）でUser OAuth Tokenを登録・置き換える。省略した設定は今の値（新規は既定値）のまま。`xoxp-`以外や無効なトークンは400`slack_token_invalid`、権限が足りなければ400`slack_scopes_missing`、Slackに届かなければ503`slack_unavailable`。Access JWTが必要 |
| `PATCH /api/v1/integrations/slack` | 本文`{immediate_notify?, detect_mentions?, detect_dms?, detect_group_dms?}`（1つ以上）で設定を変える。未接続は404。Access JWTが必要 |
| `DELETE /api/v1/integrations/slack` | 連携を解除し204を返す。未接続でも204。Access JWTが必要 |
| `GET /api/v1/integrations/chatwork` | Chatwork連携の状態。未接続は`{connected: false}`、接続済みは`{connected, account_name, organization_name, token_last4, immediate_notify, detect_mentions, detect_dms, import_tasks, status, last_error, last_synced_at, created_at}`。`last_synced_at`は検出とタスクの取り込みのうち新しい方。トークン本体は返さない。Access JWTが必要 |
| `PUT /api/v1/integrations/chatwork` | 本文`{token, immediate_notify?, detect_mentions?, detect_dms?, import_tasks?}`（16KiB以内）でAPIトークンを登録・置き換える。省略した設定は今の値（新規は既定値）のまま。形式が正しくなければ400`invalid_request`、無効なトークンは400`chatwork_token_invalid`、Chatworkに届かなければ503`chatwork_unavailable`。Access JWTが必要 |
| `PATCH /api/v1/integrations/chatwork` | 本文`{immediate_notify?, detect_mentions?, detect_dms?, import_tasks?}`（1つ以上）で設定を変える。未接続は404。Access JWTが必要 |
| `DELETE /api/v1/integrations/chatwork` | 連携を解除し204を返す。未接続でも204。Access JWTが必要 |
| `GET /api/v1/integrations/gmail` | メール連携の状態`{available, redirect_uri, max_accounts, accounts: [{id, email, immediate_notify, status, last_error, last_synced_at, created_at}]}`。`available`はGoogleのOAuthクライアントが設定されているか。未設定でも接続済みのアカウントは返す。トークンは返さない。Access JWTが必要 |
| `POST /api/v1/integrations/gmail/authorize` | 本文`{}`（追加）か`{account_id}`（再接続）でGoogleの認可を始め、同意画面のURL`{url}`を返す。未設定は409`gmail_unavailable`、10件に達していれば409`gmail_too_many`（再接続は除く）、他のユーザーのアカウントと存在しないアカウントは404。手続きは10分で無効になる。Access JWTが必要 |
| `POST /api/v1/integrations/gmail/complete` | 本文`{state, code}`か`{state, error}`でGoogleの認可を完了し、アカウントを返す。形式が正しくなければ400`invalid_request`、手続きが無効・期限切れ・使用済み・他のユーザーのものなら400`gmail_state_invalid`、拒否されたら400`gmail_denied`、`gmail.readonly`がなければ400`gmail_scope_missing`、管理者が許可していなければ403`gmail_policy_blocked`、Gmail APIが無効なら502`gmail_api_disabled`、Googleとのやりとりの失敗は502`gmail_exchange_failed`、別のユーザーが接続済みのアドレスは409`gmail_in_use`、10件に達していれば409`gmail_too_many`。Access JWTが必要 |
| `PATCH /api/v1/integrations/gmail/accounts/{id}` | 本文`{immediate_notify}`で設定を変える。他のユーザーのアカウントと存在しないアカウントは404。Access JWTが必要 |
| `DELETE /api/v1/integrations/gmail/accounts/{id}` | アカウントの接続を解除し、Googleでトークンを失効させて204を返す（失効に失敗しても204）。他のユーザーのアカウントと存在しないアカウントは404。Access JWTが必要 |
| `GET /api/v1/ai` | AIの設定。未設定は`{configured: false}`、設定済みは`{configured, provider, model, key_last4, api_key_set, account, base_url, status, last_error, updated_at}`。`provider`は`"anthropic"`、`"openai"`（ChatGPTのプラン）、`"openai_api"`（OpenAIのAPIキー）、`"openai_compatible"`（OpenAI互換の接続先）のどれか。`key_last4`はAPIキーの末尾4文字（ChatGPTのプランと、キーがないか8文字以下の場合は`""`）、`api_key_set`はAPIキーを保存しているか（ChatGPTのプランとキーのないOpenAI互換の接続先は`false`）、`account`はChatGPTのアカウントのメールアドレス（ほかの提供元と不明な場合は`null`）、`base_url`はOpenAI互換の接続先のURL（ほかの提供元と、保存した設定を読み出せない場合は`null`）。キー・トークンは返さない。Access JWTが必要 |
| `PUT /api/v1/ai` | 本文`{api_key?, model, provider?, base_url?}`（16KiB以内）でAPIキーとモデルを登録・変更する。`provider`は`"anthropic"`（省略時）、`"openai"`、`"openai_api"`、`"openai_compatible"`のどれかで、今の設定と違えば409`ai_provider_mismatch`（削除してから設定し直す）。Anthropicでは`api_key`を省くと保存済みのキーでモデルだけを変える。保存前にAnthropicで確かめ、無効なキーは400`ai_key_invalid`、モデルを使えなければ400`ai_model_unavailable`、残高・支払いの問題は400`ai_billing`、Anthropicに届かなければ503`ai_unavailable`、別の要求で設定が変わっていれば409`ai_conflict`。`"openai"`はモデルの変更だけで（接続は`/api/v1/ai/openai/*`）、`api_key`は付けられない。モデルがプランの一覧に無ければ400`ai_model_unavailable`、確認の要求が通らなければ、接続が無効なら400`ai_key_invalid`、プランで使えなければ400`ai_billing`、利用上限なら400`ai_usage_limit`、要求の形式を受け付けなければ400`ai_unsupported`、届かなければ503`ai_unavailable`。`"openai_api"`と`"openai_compatible"`では`model`（空白のない印字可能なASCIIで1〜256バイト）が必須。`"openai_api"`では`api_key`を省くか`""`にすると保存済みのキーを使い（未設定なら400`invalid_request`）、`base_url`は付けられない。`"openai_compatible"`では`base_url`（`https`、`ASTOVIA_AI_ALLOW_PRIVATE_ENDPOINTS`なら`http`も可。末尾の`/`は除く）を省くと保存済みのURLを使い、`api_key`は`""`でキーなし、省くと保存済みのキーを使う。URLが保存済みと変わる（または未設定の）ときに`api_key`を省くと400`invalid_request`。ほかの提供元に`base_url`（`null`も含む）を付けると400`invalid_request`。形式の誤りは400`invalid_request`、許可されていない`http`のURLとアドレスは400`ai_endpoint_blocked`、保存済みのキーやURLを読み出せなければ400`ai_key_invalid`。確認の要求は利用者ごとに1分10回までで、超えたら429`rate_limited`。確認の要求が通らなければ、無効なキーは400`ai_key_invalid`、モデル（互換ではURLかモデル）が見つからなければ400`ai_model_unavailable`、残高・支払いの問題は400`ai_billing`、要求の形式（JSON Schemaの構造化出力）を受け付けなければ400`ai_unsupported`、届かなければ503`ai_unavailable`。Access JWTが必要 |
| `DELETE /api/v1/ai` | AIの設定を削除し204を返す。未設定でも204。ChatGPTのプランではOpenAIでリフレッシュトークンを取り消し、取り消しに失敗したら（削除はしたうえで）200`{revoke_failed: true}`を返す。Access JWTが必要 |
| `GET /api/v1/ai/instructions` | AIの追加の指示`{instructions: {common, "github.detect", "slack.detect", "chatwork.detect", "gmail.detect", "slack.reply", "chatwork.reply", "gmail.reply"}}`。8つすべてを返し、指示のない欄は`""`。Access JWTが必要 |
| `PUT /api/v1/ai/instructions` | 本文`{instructions: {…8つすべて}}`（128KiB以内）で追加の指示をまとめて置き換え、`GET`と同じ形を返す。値はすべて文字列で必須。欠落・未知のキー・`null`・文字列以外は400`invalid_request`。各値は前後の空白を除いて保存し、空なら削除する。2000文字を超えたら400`invalid_request`。Access JWTが必要（APIトークン・MCPでは扱わない） |
| `GET /api/v1/ai/instructions/samples?scope=` | 追加の指示を試せる判定の一覧`{samples: [{id, content, status, auto_reason, conversation_name, detected_at}]}`（最大10件、新しい順）。`scope`は`common`以外の7つで、それ以外は400`invalid_request`。`conversation_name`は最初の参照の会話名か`null`、`detected_at`はRFC 3339か`null`。連携していなければ409`not_connected`。Access JWTが必要 |
| `POST /api/v1/ai/instructions/try` | 本文`{scope, item_id, common, instruction}`（すべて必須の文字列、128KiB以内）で、判定を保存済みの指示と書きかけの指示（`common`と`instruction`。前後の空白を除いて2000文字以内）でやり直し、検出は`{current: {decision, task, importance, summary, event}, draft: {…}}`（`event`は`{title, start, end}`か`null`。`start`・`end`はAIが返した現地時刻）、返信は`{current: {done}, draft: {done}}`を返す。何も保存しない。不正な本文・`scope`は400`invalid_request`、1分5回を超えたら429`rate_limited`（`Retry-After`付き）、連携していなければ409`not_connected`、試せない判定は404`not_found`、AIの設定が有効でなければ409`ai_not_ready`、利用上限で待っている間は409`ai_cooling_down`、元の会話を読み直せなければ409`source_unavailable`、自分の返信がなければ409`no_reply`、連携先の制限は429`rate_limited`、連携先の失敗は502`upstream_error`、AIの失敗は409`ai_key_invalid`・`ai_model_unavailable`・`ai_billing`・429`ai_usage_limit`・502`ai_error`、55秒を超えたら504`timeout`。Access JWTが必要 |
| `POST /api/v1/ai/openai/authorize` | 本文`{}`でChatGPTのサインインを始め、`{url, redirect_uri}`を返す。Anthropicの設定があれば409`ai_provider_mismatch`。接続済みならそのクライアントIDで、未接続なら新しいクライアントの登録として始める。手続きは10分で無効になる。Access JWTが必要 |
| `POST /api/v1/ai/openai/complete` | 本文`{callback_url}`（ブラウザが戻された`http://127.0.0.1:1455/auth/callback?…`のURL。HTTPS-Onlyモードで`https://`になったものも可）で接続を完了し、`GET /api/v1/ai`と同じ形を返す。URLを読み取れなければ400`openai_callback_invalid`（手続きは使われない）、手続きが無効・期限切れ・使用済み・他のユーザーのもの、またはOpenAIがコードを受け付けなければ400`openai_state_invalid`、拒否されたら400`openai_denied`、プランを使う許可がなければ400`openai_scope_missing`、OpenAIとのやりとりやIDトークンの検証に失敗したら502`openai_unavailable`、Anthropicの設定があれば409`ai_provider_mismatch`。モデルの確認の失敗は`PUT`と同じコード。Access JWTが必要 |
| `GET /api/v1/ai/openai/models` | ChatGPTのプランで使えるモデルの一覧`{models: [{id, name}]}`。ChatGPTのプランを設定していなければ404。エラーは`PUT`と同じコード。Access JWTが必要 |
| `GET /api/v1/push/public-key` | VAPIDの公開鍵`{public_key}`。Access JWTが必要 |
| `GET /api/v1/push/subscriptions` | 通知を受け取る端末の一覧`{subscriptions: [{id, label, created_at, last_used_at}]}`。Access JWTが必要 |
| `POST /api/v1/push/subscriptions` | 本文`{endpoint, keys: {p256dh, auth}, label?, timezone?}`で端末を登録する。新規は201、登録済みの端末は200。20台に達していると409`too_many_subscriptions`。Access JWTが必要 |
| `DELETE /api/v1/push/subscriptions/{id}` | 端末の登録を削除し204を返す。他のユーザーの端末と存在しない端末は404。Access JWTが必要 |
| `POST /api/v1/push/test` | Web Pushのテスト通知を送信待ちに入れ202を返す。端末がなければ409`no_subscription`、1分に4回目以降は429（`POST /api/v1/notification-channels/test`と合わせて数える）。Access JWTが必要 |
| `GET /api/v1/notification-settings` | 通知設定`{timezone, digest_hour, stale_days, candidate_expire_days}`。未保存なら既定値（UTC、8、3、3）。Access JWTが必要 |
| `PUT /api/v1/notification-settings` | 本文`{timezone, digest_hour, stale_days, candidate_expire_days}`（すべて必須。IANAのタイムゾーン名、0〜23、1〜30、0〜30）で保存する。`candidate_expire_days`はタスク候補を自動で対象外にするまでの日数で、0は対象外にしない。Access JWTが必要 |
| `GET /api/v1/notification-channels` | 通知の送り先`{routes: {webpush, slack, email}, slack: {configured, last_error}, email: {available, provider, address}}`。`routes`の各値は送る種類（`detected`・`stale`・`due_soon`・`overdue`・`digest`）の配列で、未保存なら`webpush`はすべて、他は空。`slack.last_error`は`webhook_invalid`か`webhook_undecryptable`かnull。メールを設定していないサーバーでは`email.available`がfalseで`provider`と`address`はnull。WebhookのURLは返さない。Access JWTが必要 |
| `PUT /api/v1/notification-channels` | 本文`{routes: {webpush, slack, email}}`（3つとも必須、16KiB以内）で送り先を保存し、GETと同じ形を返す。値は種類の配列で、重複・知らない種類・nullは400`invalid_request`。保存時に表の順に並べ直す。メールを設定していないサーバーで、保存済みにないメールの種類を足すと400`mail_unavailable`（減らすのは可）。Access JWTが必要 |
| `PUT /api/v1/notification-channels/slack-webhook` | 本文`{url}`でSlackのIncoming Webhookを登録し（暗号化して保存。送信はしない）、GETと同じ形を返す。`https://hooks.slack.com/services/…/…/…`の形でない（512バイトまで）と400`invalid_request`。Access JWTが必要 |
| `DELETE /api/v1/notification-channels/slack-webhook` | SlackのWebhookを削除し204を返す（なくても204）。種類の選択は残す。Access JWTが必要 |
| `POST /api/v1/notification-channels/test` | 本文`{channel}`（`webpush`・`slack`・`email`）の送り先にテスト通知を送信待ちに入れ202`{queued: true}`を返す。メールを設定していないサーバーで`email`は409`mail_unavailable`、SlackのWebhookがない・止まっていると409`no_destination`、`webpush`で端末がなければ409`no_subscription`。`POST /api/v1/push/test`と合わせて1分に4回目以降は429。Access JWTが必要 |
| `GET /.well-known/oauth-protected-resource/mcp` | MCPの保護リソースメタデータ（RFC 9728）。`/.well-known/oauth-protected-resource`も同じ内容 |
| `GET /.well-known/oauth-authorization-server` | 認可サーバーのメタデータ（RFC 8414） |
| `POST /oauth/register` | クライアント登録（RFC 7591）。`application/json`、64KiB以内。成功は201 |
| `GET /oauth/authorize` | 認可要求。検証後に同意ページ`/consent`へ302 |
| `POST /oauth/token` | トークン発行（`authorization_code`／`refresh_token`）。`application/x-www-form-urlencoded`、64KiB以内 |
| `POST /mcp` | MCP（Streamable HTTP、ステートレス）。OAuthのアクセストークンかAPIトークンが必要 |

LoginとRefresh（と招待の受諾・パスワード再設定の完了）は`{access_token, expires_in, user: {id, email, role, password_reset}}`を返します。`role`は`owner`か`member`、`password_reset`は直近14日以内にパスワード再設定リンクで再設定していれば最新の1件の`{at, by_email}`（発行したオーナーのメールアドレス）、なければ`null`です。

対応事項は`{id, content, status, importance, note, due_at, refs, version, created_at, updated_at, source, auto, kind, event, parent_id, parent, children}`で（`importance`は重要度で`"high"`／`"normal"`／`"low"`。作成時に省略すると`"normal"`。`parent_id`は親タスクのidか`null`、`parent`は親タスクの`{id, content, status}`か`null`、`children`は子タスクがあれば`{total, done, remaining, next_due_at}`（`total`は対象外を除いた子の数、`remaining`は未完了とタスク候補の数、`next_due_at`は未完了とタスク候補の子の一番近い期限か`null`）、なければ`null`。`status`は`open`／`candidate`（AIが見つけた未確定のタスク候補）／`done`／`dismissed`。`source`は連携が作ったものなら`"github"`か`"slack"`、それ以外は`null`。`auto`は自動で完了・対象外にしたものなら`{reason, ref_url, at}`、それ以外は`null`。`kind`は`"task"`か`"event"`（予定タスク）で、`event`は予定タスクなら`{title, start_at, end_at}`（`end_at`は終了時刻がなければ`null`）、それ以外は`null`。`kind`と`event`は作成・更新の本文では受け付けません。MCPの応答も同じ形です）、日時はUTCのRFC 3339です。`/api/v1/action-items*`はすべてBearer Access JWTまたはAPIトークンが必要で、作成・更新の本文は`application/json`の64KiB以内とします。`content`は1〜500文字（空白のみは不可）、`note`は4000文字以内、`due_at`はオフセット付きのRFC 3339、`refs`は`{service, account?, conversation_id?, conversation_name?, message_id?, url?}`の配列（10件まで、`url`はhttp／https。`conversation_name`は`#general`のような表示用の会話名で、画面ではアカウント名の代わりに表示します）、`parent_id`は自分の対応事項で親を持たないもののid（入れ子は2段までで、子を持つ対応事項には付けられません。違反は400）、`importance`は`"high"`／`"normal"`／`"low"`のいずれか（`null`やそれ以外は400）です。一覧は作成日時の降順で、同じ作成日時の間はidの降順（DBの照合順序に従う）に並びます。続きは`next_cursor`を`cursor`に渡して取得します。他のユーザーの対応事項と存在しない対応事項は、どちらも404になります。削除APIはなく、対象外は`status`の`dismissed`で表します。`status`を変更すると（REST・MCPとも）、その対応事項は連携による自動完了・再オープンの対象から外れ、`auto`は`null`に戻ります。ただし`candidate`と`open`の間の変更（タスク候補をタスクにする、その取り消し）では、Slackの返信による自動完了の対象のままです。開始時刻を過ぎた予定タスクを完了・対象外から`open`か`candidate`に戻すと、`kind`は`"task"`になります。エラーは`{error: {code, message}}`です（`/oauth/*`の処理内で返すエラーだけは、OAuthの形式`{error, error_description}`です）。認証・非公開API・MCP・`/oauth/*`の応答には`Cache-Control: no-store`を付けます。存在しないAPI・連携用URLに対して、管理画面のHTMLを返すことはありません。`//`や`..`を含む正規化されていないパスは、メソッドにかかわらず正規化後のパスへ307でリダイレクトし、`%2F`でエスケープしたスラッシュを含むパスはどのURLにも一致せず404になります。

## コード生成と検証

HCLをスキーマの正本とします。`schema.sql`とsqlcのGoパッケージは生成物です。HCLまたは別途記述した`queries.sql`を編集してから、再生成してください。

```sh
make generate
make check
```

`make generate`は`compose.test.yaml`のAtlasコンテナ（`arigaio/atlas:1.2.2`）と使い捨て開発用DBを使用し、Docker上のsqlc 1.31.1でGoコードを生成します。ホスト側のAtlas CLIは不要です。PostgreSQLの開発用DB（`atlas-dev-postgres`）は実行のたびに作り直し、終了時に削除します。SQLiteの識別子引用はsqlcと互換性のある形式へ正規化します。

`ASTOVIA_SCHEMA_DEV_URL`でPostgreSQLの開発用URLを変更する場合も、**使い捨てDBだけ**を指定してください。Atlasはスキーマの正規化中にそのDBを変更する場合があります。このURLはAtlasコンテナ内で解決するため、`compose.test.yaml`のネットワークから到達できるURL（サービス名など）だけを指定できます。`docker://`形式や`localhost`は使用できません。指定した場合、`atlas-dev-postgres`は起動しません。

`make check`はUIのビルド・型チェック・認証フローテスト、SQLite／PostgreSQLのGo競合検出付き契約テスト、`go vet`を実行します。個別に実行する場合、`make test-web`はUIの認証フローテストとビルド、`make test-go`はGoのテストだけを実行します。`make test-go`は`web/dist`が存在しないかソースより古い場合だけUIをビルドします。PostgreSQLテストは個別の一時スキーマを作成し、テスト終了後に削除します。Compose外で`ASTOVIA_TEST_POSTGRES`を指定する場合も、専用テストDBだけを使用してください。未指定の場合、PostgreSQLテストはスキップします。

検証用`compose.test.yaml`のPostgreSQLはtmpfsを使用し、ホストのループバックアドレスの55439番ポートに公開します。本番データや既存のDBは使用しません。ローカルアプリ用の永続SQLiteとは別です。検証後は次で停止できます。

```sh
docker compose -f compose.test.yaml stop postgres
```

## リリース

メンテナー向けの手順です。

1. `make check`が通ることを確認します。
2. タグを作成します: `git tag vX.Y.Z`
3. タグをpushします: `git push origin vX.Y.Z`

タグのpushで`release`ワークフローが動き、`ghcr.io/i2bskn/astovia-migrate`、`ghcr.io/i2bskn/astovia`の順に2つのイメージ（linux/amd64・linux/arm64）を公開します。テストはワークフローでは実行しません。

- タグは`vMAJOR.MINOR.PATCH`の形式です。プレリリースは`v0.2.0-rc.1`のように`-`以降を付け、`X.Y.Z-…`のタグだけを公開します（`X.Y`と`latest`は付けません）。
- 安定版は`X.Y.Z`に加え、同じ`X.Y`の中で最大のときだけ`X.Y`を、すべての安定版の中で最大のときだけ`latest`を更新します。古い系列のパッチを公開しても`latest`は動きません。
- mainに含まれないコミットのタグと、形式に合わないタグ（`vtest`、`v1.2`等）は、何も公開せずに失敗します。
- タグは1つずつpushし、前のタグのワークフローが終わってから次のタグをpushしてください。一度に4つ以上のタグをpushするとワークフローが起動しません。また、ワークフローは1つずつ順に実行し、待機できるのは1件だけです。待機中に次の実行が来ると、待機中の実行は取り消されます。取り消された実行はGitHubのActionsの画面から再実行してください。
- 失敗した場合は、同じ実行を再実行してください（同じタグを上書きします）。
- 初回の公開後に一度だけ、GitHubのパッケージ設定で`astovia`と`astovia-migrate`の2つのパッケージをpublicにしてください（公開直後はprivateです）。

## 不具合報告・要望

GitHubのIssueで受け付けます。外部からのプルリクエストは受け付けていません。

## ライセンス

[Apache License 2.0](LICENSE)で提供します。
