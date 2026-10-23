# Astovia

Astoviaは、会話から生まれる対応事項を保存する、セルフホストの統合Inboxです。現在は、埋め込み管理画面、パスワードログイン、ユーザーごとに分離したセッション、SQLite／PostgreSQLによる保存と、本人の対応事項の作成・一覧・状態変更（管理画面とREST API）を実装しています。MCP、APIトークン、外部サービス連携、AI抽出は**まだ実装していません**。

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

既存のDBを使っている場合も、ソースを更新してスキーマが変わったとき（例: 対応事項の`action_items`テーブルの追加）は、起動前にもう一度`make migrate`を実行してください。テーブルが不足していると、アプリは「database schema is not initialized or incompatible」と表示して起動しません。

生のコマンドは次のとおりです。

```sh
mkdir -p data
docker compose -f compose.test.yaml run --rm atlas schema apply --url 'sqlite://data/astovia.db' --to 'file://db/sqlite/schema.hcl' --dev-url 'sqlite://file?mode=memory'
```

### 3. 初期ユーザーを作成する

```sh
make user-create USER_EMAIL=alice@example.com
```

ログインIDはメールアドレスです。パスワードは画面に表示されない対話入力で設定します。`USER_EMAIL`はmakeのコマンドライン引数で指定してください。シェルの環境変数として設定した`USER_EMAIL`は使用せず、未指定時と同じく何も実行せずに失敗します。make経由で使える文字は英数字と`._%+@-`だけで、`@`を含まない値や3〜254文字に収まらない値も実行前に拒否します。それ以外の記号を含むアドレスは`astovia users ... --email`を直接使ってください。形式の検証はCLIが行います。

ログインIDをメールアドレスに変更する前に作成したSQLiteのDBは、`users`表の列が異なるため起動時のスキーマ確認で失敗します。開発中のデータしか無い場合は、DBファイルを削除して`make migrate`で作り直し、`make user-create USER_EMAIL=...`でユーザーを作り直してください。自動移行は用意していません。旧DBに対して`make migrate`を実行すると、`username`列の削除と`email`列の追加の計画が表示されます。データが残らないため承認せず、DBファイルを作り直してください。

ユーザー操作は`docker compose run --rm app go run ./cmd/astovia users ...`で、イメージ内の古いバイナリではなく現在のソースから実行します。そのため、CLIのコードを変更してもイメージの再ビルドは不要です。ユーザー操作CLIには署名鍵は不要ですが、Airから起動する`serve`では必須です。

### 4. 起動・確認・停止する

```sh
make up
make logs
```

[http://localhost:5173/admin/](http://localhost:5173/admin/)を開いてログインしてください。ホストへ公開するのはViteの`127.0.0.1:5173`だけです。Goはコンテナ内の`0.0.0.0:8080`で待ち受け、Viteが`/auth`・`/api`・`/healthz`を転送します。ブラウザからのOriginは変更せず、Go側の公開URLも`http://localhost:5173`にそろえます。`localhost`と`127.0.0.1`は異なるOriginのため、ブラウザでは`localhost`を使用してください。

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

`Dockerfile`の既定の最終段は、画面を埋め込んだ静的バイナリとCA証明書だけを含む非root実行の`scratch`イメージです。`docker build -t astovia .`で作成できます。ローカルComposeは、この最終段ではなく開発専用の`development`／`frontend-development`段を使用します。

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

`compose.yaml`では、署名鍵とUID／GIDだけを外部から展開し、待ち受けアドレス・公開URL・SQLite接続先はファイル内の固定値を使用します。Composeの公開URLは`http://localhost:5173`で、直接起動時の既定値とは異なります。

ホスト側でViteを使う場合は`web`で`pnpm dev`を実行し、Go側の`ASTOVIA_PUBLIC_URL`も`http://localhost:5173`に設定してください。ViteのAPI転送先は既定で`http://localhost:8080`です。開発時だけ`ASTOVIA_DEV_API_URL`で転送先、`ASTOVIA_DEV_POLLING=true`でポーリング監視を指定できます。

## PostgreSQLを使用する

以下はアプリを直接起動する場合の設定です。`compose.yaml`はSQLiteを使用する構成です。この手順ではホスト側のAtlas CLIを使用します。

`ASTOVIA_DB_DRIVER=postgres`と、PostgreSQLの接続URLを`ASTOVIA_DB_DSN`へ明示的に設定します。PostgreSQLの接続先を省略してもSQLiteへは切り替わりません。起動やユーザー操作の前に、同じ接続先へAtlasで`db/postgres/schema.hcl`を適用してください。

Atlasの`--dev-url`には、アプリのDBではなく、**別の使い捨て開発用DB**を指定します。

```sh
export ASTOVIA_DB_DRIVER=postgres
export ASTOVIA_DB_DSN='postgres://USER:PASSWORD@HOST/DATABASE?sslmode=require'
atlas schema apply --url "$ASTOVIA_DB_DSN" --to file://db/postgres/schema.hcl --dev-url 'docker://postgres/17/dev?search_path=public'
```

更新時は変更計画を一つずつ確認し、永続データをバックアップしてから適用してください。DBドライバーの設定変更だけでは、既存データは別のDBへ移行されません。

## ユーザーを管理する

公開登録画面とメールによる復旧機能はありません。選択したDBにアクセスできる運用者が、初期ユーザーと追加ユーザーを作成します。

Dockerで起動している場合は、同じCompose構成で操作します。現在のソースから`go run`で実行するため、イメージの再ビルドは不要です。

```sh
make user-create USER_EMAIL=alice@example.com
make user-reset-password USER_EMAIL=alice@example.com
make user-disable USER_EMAIL=alice@example.com
```

ネイティブバイナリでは次のように実行します。

```sh
./bin/astovia users create --email alice@example.com
./bin/astovia users reset-password --email alice@example.com
./bin/astovia users disable --email alice@example.com
```

パスワードは端末の表示を無効にして読み取ります。非対話で実行する場合は`--password-stdin`を指定し、標準入力から1行で渡してください。末尾の改行1つ（`\n`、`\r\n`、`\r`）は除去します。複数行の入力と75バイトを超える入力は拒否します。パスワードをコマンドライン引数には含めません。Composeへパイプで渡す場合は、`docker compose run --rm -T app go run ./cmd/astovia users create --email alice@example.com --password-stdin`のように`-T`を付けて実行します。

メールアドレスは前後のASCII空白を除去して小文字に変換します（ローカル部も小文字にします）。ASCIIのみで254文字以内、`@`より前のローカル部は64文字以内です。形式はHTMLの`input type=email`と同じ規則で、`alice@localhost`のようにドットを含まないドメインも受け付けます。国際化ドメイン名はUnicodeのままでは受け付けないため、punycode（`xn--…`）で指定してください。確認メールは送信しません。パスワードはUTF-8で12〜72バイトです。同じメールアドレス（大文字・小文字の違いを含む）の作成は拒否します。パスワード再設定とユーザー無効化は全セッションを失効させます。再設定によって無効化済みユーザーが再び有効になることはありません。CLIはサーバーと同じDB設定を使い、JWT署名鍵は必要としません。

## 認証と運用上の制約

- Access JWTの有効期間は最大10分で、ブラウザのメモリだけに保持します。Refreshセッションは作成から7日で失効し、ローテーションでは延長しません。
- Refresh Tokenは乱数で生成し、DBにはハッシュのみを保存します。Cookieは`HttpOnly`、`SameSite=Strict`、`Path=/auth`とし、HTTPSでは`Secure`も付けます。再利用検出のために使用済みトークンのハッシュを保持します。現時点ではセッション履歴の削除コマンドがないため、セッションや更新の回数に応じて保存量が増えます。
- 使用済みRefresh Tokenを再利用すると、そのセッションを失効させます。対応ブラウザではWeb Locksでタブ間の認証操作を直列化します。Web Locksがない環境では、同時操作によって再ログインが必要になる場合があります。
- 非公開APIへのリクエストごとに、DB上のユーザーとセッションが有効か確認します。ログアウト、パスワード再設定、ユーザー無効化、再利用検出の後は、発行済みJWTも次の検証から拒否します。実行中のリクエストを遡って取り消すことはできません。
- ブラウザ向け認証POSTは、設定されたOriginとの完全一致、`application/json`、JSONオブジェクトを必須とします。RefreshとLogoutの本文は`{}`です。コマンドラインからのリクエストも、Originがなければ拒否します。Refresh失敗時にはCookieを消さず、遅れて返った失敗応答が新しいCookieを削除することを防ぎます。
- ログインは接続元IPごとに1分間で10回までです。IPv6は`/64`単位でまとめて数え、IPv4を埋め込んだIPv6はIPv4として扱います。記録件数に上限（4096件）のあるプロセス内テーブルを使用し、転送ヘッダーは信頼しません。上限に達した場合は新しい接続元を拒否せず、制限中でない最も古い記録（すべて制限中なら最も古い記録）を削除して受け付けます。このため、上限を超える数の接続元を用意できる攻撃者に対しては、毎分の総試行回数に上限がなくなります。実質的な上限はbcrypt（コスト12）のCPU負荷で、パスワードは12バイト以上を必須としています。全利用者がログインできなくなる事態を避けることを優先しています。リバースプロキシ配下の利用者はプロキシのIPにまとめて制限され、複数インスタンス間では制限を共有しません。利用範囲を広げる際は、手前のプロキシ等にも適切なレート制限を設定してください。
- リバースプロキシを使用する場合は、`ASTOVIA_PUBLIC_URL`にHTTPSのOriginを設定します。パス、認証情報、クエリ、フラグメントは指定できません。プロキシでTLSを終端し、バックエンドへの直接接続を制限してください。HTTPの公開URLはループバック開発用途だけに限ります。
- ホスト名はASCIIのDNS名か明示的なIPアドレスを使用します。国際化ドメイン名はpunycodeで指定してください。数値ホストの省略表記、スコープ付きIPv6、IPv4を埋め込んだIPv6は拒否します。DNS名の大文字・小文字、標準ポート、ポートの数値表記、IPv6の表記は設定読込時に正規化し、Origin検証とJWT issuerで共用します。以前の正規化されていないissuerで発行したトークンは再ログインが必要です。正規化済みの設定には影響しません。

`/admin/`という名前によって、役割管理、共有Inbox、他ユーザーの非公開情報へのアクセス権が付くことはありません。この初期実装には本番デプロイの設定を含みません。

## URLと応答

| URL | 動作 |
| --- | --- |
| `GET /` | `/admin/`へリダイレクト |
| `GET /healthz` | プロセスの稼働確認のみ。非公開情報は返さない |
| `GET /admin/*` | 埋め込み管理画面。存在しないアセットは404 |
| `POST /auth/login` | 本文は`{ "email": "alice@example.com", "password": "…" }` |
| `POST /auth/refresh` | Cookieのトークンを更新。本文は`{}` |
| `POST /auth/logout` | セッション失効とCookie削除。本文は`{}` |
| `GET /api/v1/me` | 本人の`{id, email}`。Bearer Access JWTが必要 |
| `GET /api/v1/action-items` | 本人の対応事項の一覧。クエリは`status`（`open`／`done`／`dismissed`）、`limit`（1〜100、既定50）、`cursor`。`{items, next_cursor}`を返す |
| `POST /api/v1/action-items` | 対応事項を作成し201と`Location`を返す。本文は`{content, status?, note?, due_at?, refs?}` |
| `GET /api/v1/action-items/{id}` | 本人の対応事項を1件返す |
| `PATCH /api/v1/action-items/{id}` | 指定したキーだけを更新する。`note`／`due_at`は`null`で消去。`version`を付けると一致しない場合に409 |

LoginとRefreshは`{access_token, expires_in, user: {id, email}}`を返します。

対応事項は`{id, content, status, note, due_at, refs, version, created_at, updated_at}`で、日時はUTCのRFC 3339です。`/api/v1/action-items*`はすべてBearer Access JWTが必要で、作成・更新の本文は`application/json`の64KiB以内とします。`content`は1〜500文字（空白のみは不可）、`note`は4000文字以内、`due_at`はオフセット付きのRFC 3339、`refs`は`{service, account?, conversation_id?, message_id?, url?}`の配列（10件まで、`url`はhttp／https）です。一覧は作成日時の降順で、同じ作成日時の間はidの降順（DBの照合順序に従う）に並びます。続きは`next_cursor`を`cursor`に渡して取得します。他のユーザーの対応事項と存在しない対応事項は、どちらも404になります。削除APIはなく、対象外は`status`の`dismissed`で表します。エラーは`{error: {code, message}}`です。認証・非公開APIの応答には`Cache-Control: no-store`を付けます。存在しないAPI・MCP・連携用URLに対して、管理画面のHTMLを返すことはありません。`//`や`..`を含む正規化されていないパスは、メソッドにかかわらず正規化後のパスへ307でリダイレクトし、`%2F`でエスケープしたスラッシュを含むパスはどのURLにも一致せず404になります。

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

## 不具合報告・要望

GitHubのIssueで受け付けます。外部からのプルリクエストは受け付けていません。

## ライセンス

[Apache License 2.0](LICENSE)で提供します。
