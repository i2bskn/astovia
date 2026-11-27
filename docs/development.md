# 開発

Astoviaを開発する場合の、ローカル開発・コード生成と検証・リリースの手順をまとめます。

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

`data/`を作成し、Atlasコンテナで`db/sqlite/schema.hcl`を`data/astovia.db`へ適用します。Atlasが表示する変更計画を確認してから承認してください（自動承認はしません）。アプリの起動時にスキーマは自動更新しません。対象はSQLiteだけです。PostgreSQLへの適用は[「PostgreSQLを使用する」](https://astovia.dev/guides/native/#postgresqlを使用する)の手順で行ってください。

既存のDBを使っている場合も、ソースを更新してスキーマが変わったとき（例: 対応事項の`action_items`テーブル、APIトークンの`api_tokens`テーブル、MCPのOAuthの`oauth_clients`・`oauth_grants`・`oauth_tokens`テーブルの追加、GitHub連携と通知の`github_connections`・`web_push_subscriptions`・`notification_settings`・`notifications`・`instance_secrets`テーブルと`action_items`の列の追加、AIの設定の`ai_settings`テーブルの追加、Slack連携の`slack_connections`テーブルの追加、Chatwork連携の`chatwork_connections`テーブルの追加、メール連携の`gmail_accounts`・`google_auth_states`テーブルの追加、`action_items`の`importance`列の追加、AIの追加の指示の`ai_instructions`テーブルの追加、通知の送り先の`notification_channels`テーブルと`notifications`の`channel`列の追加、`users`の`oidc_issuer`・`oidc_subject`列の追加、二段階認証の`mfa_totp`・`mfa_webauthn_credentials`・`mfa_recovery_codes`テーブルの追加、authlumeへの認証の置き換えによる`web_sessions`・`oidc_logins`・`webauthn_ceremonies`・`mcp_oauth_codes`・`mcp_oauth_connections`・`mcp_oauth_registrations`・`mcp_oauth_client_labels`テーブルと`users`の`oidc_link_approved_until`列などの追加）は、起動前にもう一度`make migrate`を実行してください。起動時のスキーマ確認はこれらのテーブルも対象にするため、テーブルを追加する前のDBでは、migrateするまで起動しません。

認証の置き換えでは、Atlasが以前の認証のテーブル（`sessions`・`refresh_tokens`・`oauth_clients`・`oauth_grants`・`oauth_tokens`・`mfa_used_challenges`）の削除を計画します。承認して構いません。適用後は全員がログインし直し、MCPのクライアントは接続し直します。パスワードのハッシュはArgon2idだけを受け付けるため、以前のパスワードではログインできません。開発用のユーザーは`make user-reset-password USER_EMAIL=...`でパスワードを設定し直してください。以前の版で登録した認証アプリとパスキー・セキュリティキーは使えず、`astovia users reset-mfa`で解除して登録し直します（詳しくは[リリースノート](release-notes.md)）。テーブルが不足していると、アプリは「database schema is not initialized or incompatible」と表示して起動しません。

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

OIDCでログインする構成（`ASTOVIA_AUTH_MODE=oidc`）は`ASTOVIA_PUBLIC_URL`が`https`のときだけ使えます。このローカル開発構成（`http://localhost:5173`）では起動時のエラーになります。https で動かす場合は、最初のオーナーを`--no-password`でパスワードなしに作成します。作成と同時に、OIDCでの紐付けを7日間承認します。その間の初回のOIDCログインで、確認済みのメールアドレスが一致するユーザーにIDトークンの`iss`と`sub`が紐付き、承認は消えます。2回目以降は`iss`と`sub`だけで本人を特定します。承認のないユーザーには、メールアドレスが一致するだけでは紐付きません（`not_allowed`）。期限が切れた場合や、パスワードでログインしていたユーザーは、`users approve-oidc --email ADDRESS`で7日間承認し直します。OIDCの構成ではAstoviaの二段階認証は適用しません（IdPの多要素認証を使ってください）。makeのターゲットはないため、生のコマンドで実行します。

```sh
docker compose run --rm app go run ./cmd/astovia users create --email alice@example.com --no-password
```

IdP側の識別子が変わると、紐付け済みのユーザーはログインできなくなります。該当するのは、`ASTOVIA_OIDC_ISSUER`を変えた場合・クライアントIDごとに`sub`が変わるIdPでクライアントを作り直した場合・IdPでアカウントを作り直した場合です。Cloudflare Access for SaaSのissuerはクライアントIDを含むため、Accessのアプリケーションを作り直すと変わります。その場合は紐付けを解除し、`approve-oidc`で承認してから、次のログインで紐付け直します。

```sh
docker compose run --rm app go run ./cmd/astovia users unlink --email alice@example.com
docker compose run --rm app go run ./cmd/astovia users unlink --all
docker compose run --rm app go run ./cmd/astovia users approve-oidc --email alice@example.com
```

`unlink`は紐付けを外し、そのユーザーのセッション・APIトークン・MCPの接続をすべて失効させます。紐付け直す承認は付けません。`approve-oidc`で承認したユーザーには、7日以内に確認済みメールアドレスが一致してログインした人が紐付きます。IdP側でメールアドレスが変わった人は紐付け直せません（Astoviaにメールアドレスを変える機能はありません）。以前のメールアドレスをIdPで別の人が使っている場合は、その人がそのユーザーに紐付くため、承認する前に確かめてください。ユーザーの無効化（`users disable`と画面）は承認も消します。

### v0.1.0から更新する場合（OIDC）

`ASTOVIA_ALLOWED_EMAILS`は廃止しました。値が残っていると`serve`は起動せず、手順を表示して止まります（`users`のCLIは値が残っていても動きます）。ログインできる人はユーザー管理画面の招待で決まり、招待のない人は初回ログインで追加されません。

v0.1.0では、許可リストから外した既存ユーザーもログインを止められていました。更新後、紐付けのない既存ユーザーは`users approve-oidc`で承認するまでOIDCでログインできません。承認したユーザーは確認済みのメールアドレスの一致で紐付き、元オーナーはオーナーとして戻ります。次の順に進めてください。

1. スキーマを適用します（起動前に必要です。配布構成では`docker compose run --rm migrate`、PostgreSQLでは既存の手順のまま）。
2. 許可リストから外して止めていた人を無効化します: `astovia users disable --email <address>`
3. `ASTOVIA_ALLOWED_EMAILS`を消します。
4. 最初のオーナーがいなければ、`astovia users create --email <address> --no-password`で作成します。既存のユーザーは`astovia users approve-oidc --email <address>`で承認します（7日間）。
5. 他の人をユーザー管理画面から招待します。

CLIを実行しにくい構成では、更新する前（v0.1.0のうち）にユーザー管理画面で無効化しておいてください。

issuerがGoogle（`https://accounts.google.com`）の場合、`@gmail.com`以外のアドレスの初回ログインにはGoogle Workspaceの`hd`クレームが必要です。v0.1.0でログインしていた「独自ドメインのアドレスで作った個人のGoogleアカウント」は、更新後は紐付けられません。回避する設定はありません。Google以外のissuerでは`ASTOVIA_OIDC_REQUIRE_GOOGLE_HD=true`で同じ確認をします。issuerがGoogleのときに`false`を設定すると起動エラーになります。

ユーザー操作は`docker compose run --rm app go run ./cmd/astovia users ...`で、イメージ内の古いバイナリではなく現在のソースから実行します。そのため、CLIのコードを変更してもイメージの再ビルドは不要です。ユーザー操作CLIには署名鍵は不要ですが、Airから起動する`serve`では必須です。

### 4. 起動・確認・停止する

```sh
make up
make logs
```

[http://localhost:5173/](http://localhost:5173/)を開いてログインしてください。ホストへ公開するのはViteの`127.0.0.1:5173`だけです。Goはコンテナ内の`0.0.0.0:8080`で待ち受け、Viteが`/auth`・`/api`・`/healthz`・`/mcp`・`/oauth`・`/.well-known`を転送します。ブラウザからのOriginは変更せず、Go側の公開URLも`http://localhost:5173`にそろえます。`localhost`と`127.0.0.1`は異なるOriginのため、ブラウザでは`localhost`を使用してください。

ログインのCookie`__Host-astovia_session`には`Secure`属性が付きます。ブラウザは`http://localhost`を安全な文脈として扱うため、ChromeとFirefoxではhttpのままでも保存します。Safariでは確認していません。ログインしてもすぐログイン画面に戻る場合は、Cookieが保存されていません。公開URLが`http`のこの構成では、パスキー・セキュリティキー、MCPのOAuth（`/oauth/*`と同意画面`/oauth/authorize`はJSONの404で、`/mcp`はAPIトークンだけ）、OIDCのログインは使えません。パスワードのログイン、認証アプリ、リカバリーコード、APIトークンは使えます。

停止する場合は次を実行します。コンテナは削除せずに停止します（`docker compose stop`）。

```sh
make stop
```

データはホストの`./data`に保存され、コンテナの再ビルドや削除では消えません。通常のGo・TypeScript／TSX・CSSの変更は、ソースの読み取り専用マウントを通じて自動反映されます。Goの再起動中はAPIが一時的に応答できない場合があります。画面側はReact Fast Refreshで更新し、ページ全体を再読み込みした場合は`GET /api/v1/me`でログイン状態を確かめます（セッションはCookieにあるため、再読み込みでログアウトしません）。

Dockerのファイル通知差異を避けるため、AirとViteのファイル監視にはポーリングを使用します。Goのビルドキャッシュ（`GOCACHE=/tmp/go-build`）は、`app`の`/tmp`にマウントしたDockerの名前付きボリューム`go-build-cache`に保存し、`docker compose run --rm`を繰り返しても再利用します。Viteのキャッシュは開発イメージ内の`node_modules`に保存します。ホスト側で事前に生成した`web/dist`や`node_modules`は不要です。

依存関係の変更、DockerfileやCompose設定の変更には、再ビルドして起動し直してください。`.air.toml`等の監視設定を変更した場合も、開発プロセスを再起動してください。

```sh
docker compose up -d --build
```

この構成はローカル開発用です。本番公開の設定やデプロイは含みません。スキーマの適用は引き続きAtlasで明示的に行い、ホットリロードではDBを更新しません。

`Dockerfile`の既定の最終段は、画面を埋め込んだ静的バイナリとCA証明書だけを含む非root実行の`scratch`イメージです。`docker build -t astovia .`で作成できます。ローカルComposeは、この最終段ではなく開発専用の`development`／`frontend-development`段を使用します。`migrate`段は公開用のスキーマ適用イメージです（[「本番で動かす」](https://astovia.dev/guides/requirements/)を参照）。

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

タグのpushで`release`ワークフローが動き、`ghcr.io/i2bskn/astovia-migrate`、`ghcr.io/i2bskn/astovia`の順に2つのイメージ（linux/amd64・linux/arm64）を公開します。公開の前にワークフローでも`make check`を実行し、失敗したら何も公開しません。

- タグは`vMAJOR.MINOR.PATCH`の形式です。プレリリースは`v0.2.0-rc.1`のように`-`以降を付け、`X.Y.Z-…`のタグだけを公開します（`X.Y`と`latest`は付けません）。
- 安定版は`X.Y.Z`に加え、同じ`X.Y`の中で最大のときだけ`X.Y`を、すべての安定版の中で最大のときだけ`latest`を更新します。古い系列のパッチを公開しても`latest`は動きません。
- mainに含まれないコミットのタグと、形式に合わないタグ（`vtest`、`v1.2`等）は、何も公開せずに失敗します。
- タグは1つずつpushし、前のタグのワークフローが終わってから次のタグをpushしてください。一度に4つ以上のタグをpushするとワークフローが起動しません。また、ワークフローは1つずつ順に実行し、待機できるのは1件だけです。待機中に次の実行が来ると、待機中の実行は取り消されます。取り消された実行はGitHubのActionsの画面から再実行してください。
- 失敗した場合は、同じ実行を再実行してください（同じタグを上書きします）。
- 初回の公開後に一度だけ、GitHubのパッケージ設定で`astovia`と`astovia-migrate`の2つのパッケージをpublicにしてください（公開直後はprivateです）。
