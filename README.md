# Astovia

Astoviaは、会話から生まれる対応事項を保存する、セルフホストの統合Inboxです。現在はアプリケーションの土台として、埋め込み管理画面、パスワードログイン、ユーザーごとに分離したセッション、SQLite／PostgreSQLによる保存を実装しています。対応事項、MCP、APIトークン、外部サービス連携、AI抽出は**まだ実装していません**。

## Dockerでローカル開発する

Docker Composeと、ホスト側のAtlas CLIが必要です。この開発方法ではホスト側のGo・Node.js・pnpmは不要です。GoはAirでソースの変更を検知して再ビルド・再起動し、React／TypeScript／CSSはViteの開発サーバーで変更を反映します。

以下はリポジトリのルートで実行してください。`compose.yaml`はAirとViteを動かすローカル開発用、`compose.test.yaml`はテスト・ビルド・コード生成用です。

### 1. 保存先とスキーマを準備する

```sh
mkdir -p data
export LOCAL_UID="$(id -u)"
export LOCAL_GID="$(id -g)"
atlas schema apply --url 'sqlite://data/astovia.db' --to 'file://db/sqlite/schema.hcl' --dev-url 'sqlite://file?mode=memory'
```

Atlasが表示する変更計画を確認してから適用してください。アプリの起動時にスキーマは自動更新しません。コンテナも同じ`data/astovia.db`を使用します。`LOCAL_UID`と`LOCAL_GID`をホストのユーザーに合わせることで、ホストで作成したDBをコンテナから更新できます。省略時は`1000:1000`です。

### 2. 署名鍵を用意する

初回のみ、署名鍵を生成します。

```sh
export ASTOVIA_JWT_SECRET="$(openssl rand -base64 32)"
```

生成した値は安全な場所に保存し、再起動時も同じ値を設定してください。値は32バイト以上の乱数を標準のパディング付きBase64で表したものです。Gitへ追加しないでください。

Docker Composeはシェルの環境変数またはリポジトリルートの`.env`から値を展開します。`.env`を使う場合は、生成した値を`ASTOVIA_JWT_SECRET=...`として保存し、`LOCAL_UID`と`LOCAL_GID`も必要に応じて設定してください。既存の`.env`がある場合は上書きせず、内容を確認して追記・変更してください。`.env.example`に設定例があります。

### 3. ビルドして初期ユーザーを作成する

```sh
docker compose build
docker compose run --rm --entrypoint /out/astovia app users create --username alice
```

パスワードは画面に表示されない対話入力で設定します。ユーザー操作CLIには署名鍵は不要ですが、Airから起動する`serve`では必須です。`--entrypoint /out/astovia`で、Airではなくイメージ内のCLIを直接実行します。このCLIはイメージをビルドした時点のコードなので、CLIのコードを変更した場合はイメージも再ビルドしてください。

### 4. 起動・確認・停止する

```sh
docker compose up -d
docker compose logs -f app web
```

[http://localhost:5173/admin/](http://localhost:5173/admin/)を開いてログインしてください。ホストへ公開するのはViteの`127.0.0.1:5173`だけです。Goはコンテナ内の`0.0.0.0:8080`で待ち受け、Viteが`/auth`・`/api`・`/healthz`を転送します。ブラウザからのOriginは変更せず、Go側の公開URLも`http://localhost:5173`にそろえます。`localhost`と`127.0.0.1`は異なるOriginのため、ブラウザでは`localhost`を使用してください。

停止する場合は次を実行します。

```sh
docker compose stop
```

データはホストの`./data`に保存され、コンテナの再ビルドや削除では消えません。通常のGo・TypeScript／TSX・CSSの変更は、ソースの読み取り専用マウントを通じて自動反映されます。Goの再起動中はAPIが一時的に応答できない場合があります。画面側はReact Fast Refreshで更新し、ページ全体を再読み込みした場合は既存のRefresh処理でログイン状態を復元します。

Dockerのファイル通知差異を避けるため、AirとViteのファイル監視にはポーリングを使用します。Goのビルド出力とキャッシュはコンテナ内の一時領域、Viteのキャッシュは開発イメージ内の`node_modules`に保存します。ホスト側で事前に生成した`web/dist`や`node_modules`は不要です。

依存関係の変更、DockerfileやCompose設定の変更、イメージ内CLIへの変更反映には、再ビルドして起動し直してください。`.air.toml`等の監視設定を変更した場合も、開発プロセスを再起動してください。

```sh
docker compose up -d --build
```

この構成はローカル開発用です。本番公開の設定やデプロイは含みません。スキーマの適用は引き続きAtlasで明示的に行い、ホットリロードではDBを更新しません。

`Dockerfile`の既定の最終段は、画面を埋め込んだ静的バイナリとCA証明書だけを含む非root実行の`scratch`イメージです。`docker build -t astovia .`で作成できます。ローカルComposeは、この最終段ではなく開発専用の`development`／`frontend-development`段を使用します。

## ネイティブバイナリをビルドする

コンテナを使わずにアプリ本体を直接起動する場合、Go 1.26.4以降が必要です。以下のビルド手順はフロントエンドのビルド・テストにNode.js 22のDockerコンテナを使用します。フロントエンドをホスト側で開発する場合はNode.js 22とpnpm 10.28.1も用意してください。

```sh
make build
```

`bin/astovia`にホスト向けバイナリを生成します。フロントエンドの生成物はGit管理対象外なので、取得直後にはビルドが必要です。AIの認証情報は起動に不要です。

### SQLiteで直接起動する

リポジトリのルートで実行してください。SQLiteの相対パスは、プロセスの作業ディレクトリを基準に解決します。

```sh
mkdir -p data
atlas schema apply --url 'sqlite://data/astovia.db' --to 'file://db/sqlite/schema.hcl' --dev-url 'sqlite://file?mode=memory'
./bin/astovia users create --username alice
# 初回に保存した署名鍵をシェルのASTOVIA_JWT_SECRETへ設定してから起動します。
./bin/astovia serve
```

Docker起動手順で同じDBとユーザーを作成済みの場合、作成操作を繰り返す必要はありません。直接起動するアプリは`.env`を自動で読み込みません。必要な値をシェルまたは実行環境の環境変数として設定してください。

既定値は次のとおりです。

| 環境変数 | 既定値 |
| --- | --- |
| `ASTOVIA_ADDR` | `127.0.0.1:8080` |
| `ASTOVIA_PUBLIC_URL` | `http://localhost:8080` |
| `ASTOVIA_DB_DRIVER` | `sqlite` |
| `ASTOVIA_DB_DSN` | `file:./data/astovia.db` |
| `ASTOVIA_JWT_SECRET` | なし。`serve`で必須 |

`compose.yaml`では、署名鍵とUID／GIDだけを外部から展開し、待ち受けアドレス・公開URL・SQLite接続先はファイル内の固定値を使用します。Composeの公開URLは`http://localhost:5173`で、直接起動時の既定値とは異なります。

ホスト側でViteを使う場合は`web`で`pnpm dev`を実行し、Go側の`ASTOVIA_PUBLIC_URL`も`http://localhost:5173`に設定してください。ViteのAPI転送先は既定で`http://localhost:8080`です。開発時だけ`ASTOVIA_DEV_API_URL`で転送先、`ASTOVIA_DEV_POLLING=true`でポーリング監視を指定できます。

## PostgreSQLを使用する

以下はアプリを直接起動する場合の設定です。`compose.yaml`はSQLiteを使用する構成です。

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

Dockerで起動している場合は、同じCompose構成で操作します。

```sh
docker compose run --rm --entrypoint /out/astovia app users create --username alice
docker compose run --rm --entrypoint /out/astovia app users reset-password --username alice
docker compose run --rm --entrypoint /out/astovia app users disable --username alice
```

ネイティブバイナリでは次のように実行します。

```sh
./bin/astovia users create --username alice
./bin/astovia users reset-password --username alice
./bin/astovia users disable --username alice
```

パスワードは端末の表示を無効にして読み取ります。非対話で実行する場合は`--password-stdin`を指定し、標準入力から渡してください。パスワードをコマンドライン引数には含めません。Composeへパイプで渡す場合は、`run --rm -T --entrypoint /out/astovia app ... --password-stdin`を使用します。

ユーザー名は前後の空白を除去して小文字に変換し、英数字で始まる3〜64文字のASCII英数字・ピリオド・アンダースコア・ハイフンを受け付けます。パスワードはUTF-8で12〜72バイトです。同じユーザー名の作成は拒否します。パスワード再設定とユーザー無効化は全セッションを失効させます。再設定によって無効化済みユーザーが再び有効になることはありません。CLIはサーバーと同じDB設定を使い、JWT署名鍵は必要としません。

## 認証と運用上の制約

- Access JWTの有効期間は最大10分で、ブラウザのメモリだけに保持します。Refreshセッションは作成から7日で失効し、ローテーションでは延長しません。
- Refresh Tokenは乱数で生成し、DBにはハッシュのみを保存します。Cookieは`HttpOnly`、`SameSite=Strict`、`Path=/auth`とし、HTTPSでは`Secure`も付けます。再利用検出のために使用済みトークンのハッシュを保持します。現時点ではセッション履歴の削除コマンドがないため、セッションや更新の回数に応じて保存量が増えます。
- 使用済みRefresh Tokenを再利用すると、そのセッションを失効させます。対応ブラウザではWeb Locksでタブ間の認証操作を直列化します。Web Locksがない環境では、同時操作によって再ログインが必要になる場合があります。
- 非公開APIへのリクエストごとに、DB上のユーザーとセッションが有効か確認します。ログアウト、パスワード再設定、ユーザー無効化、再利用検出の後は、発行済みJWTも次の検証から拒否します。実行中のリクエストを遡って取り消すことはできません。
- ブラウザ向け認証POSTは、設定されたOriginとの完全一致、`application/json`、JSONオブジェクトを必須とします。RefreshとLogoutの本文は`{}`です。コマンドラインからのリクエストも、Originがなければ拒否します。Refresh失敗時にはCookieを消さず、遅れて返った失敗応答が新しいCookieを削除することを防ぎます。
- ログインは接続元IPごとに1分間で10回までです。記録件数に上限のあるプロセス内テーブルを使用し、転送ヘッダーは信頼しません。リバースプロキシ配下の利用者はプロキシのIPにまとめて制限され、複数インスタンス間では制限を共有しません。利用範囲を広げる際は、手前のプロキシ等にも適切なレート制限を設定してください。
- リバースプロキシを使用する場合は、`ASTOVIA_PUBLIC_URL`にHTTPSのOriginを設定します。パス、認証情報、クエリ、フラグメントは指定できません。プロキシでTLSを終端し、バックエンドへの直接接続を制限してください。HTTPの公開URLはループバック開発用途だけに限ります。
- ホスト名はASCIIのDNS名か明示的なIPアドレスを使用します。国際化ドメイン名はpunycodeで指定してください。数値ホストの省略表記、スコープ付きIPv6、IPv4を埋め込んだIPv6は拒否します。DNS名の大文字・小文字、標準ポート、ポートの数値表記、IPv6の表記は設定読込時に正規化し、Origin検証とJWT issuerで共用します。以前の正規化されていないissuerで発行したトークンは再ログインが必要です。正規化済みの設定には影響しません。

`/admin/`という名前によって、役割管理、共有Inbox、他ユーザーの非公開情報へのアクセス権が付くことはありません。この初期実装には本番デプロイの設定を含みません。

## URLと応答

| URL | 動作 |
| --- | --- |
| `GET /` | `/admin/`へリダイレクト |
| `GET /healthz` | プロセスの稼働確認のみ。非公開情報は返さない |
| `GET /admin/*` | 埋め込み管理画面。存在しないアセットは404 |
| `POST /auth/login` | 本文は`{ "username": "alice", "password": "…" }` |
| `POST /auth/refresh` | Cookieのトークンを更新。本文は`{}` |
| `POST /auth/logout` | セッション失効とCookie削除。本文は`{}` |
| `GET /api/v1/me` | 本人の`{id, username}`。Bearer Access JWTが必要 |

LoginとRefreshは`{access_token, expires_in, user: {id, username}}`を返します。エラーは`{error: {code, message}}`です。認証・非公開APIの応答には`Cache-Control: no-store`を付けます。存在しないAPI・MCP・連携用URLに対して、管理画面のHTMLを返すことはありません。

## コード生成と検証

HCLをスキーマの正本とします。`schema.sql`とsqlcのGoパッケージは生成物です。HCLまたは別途記述した`queries.sql`を編集してから、再生成してください。

```sh
make generate
make check
```

`make generate`はAtlasと使い捨て開発用DBを使用し、Docker上のsqlc 1.31.1でGoコードを生成します。SQLiteの識別子引用はsqlcと互換性のある形式へ正規化します。`ASTOVIA_SCHEMA_DEV_URL`でPostgreSQLの開発用URLを変更する場合も、**使い捨てDBだけ**を指定してください。Atlasはスキーマの正規化中にそのDBを変更する場合があります。

`make check`はUIのビルド・型チェック・認証フローテスト、SQLite／PostgreSQLのGo競合検出付き契約テスト、`go vet`を実行します。PostgreSQLテストは個別の一時スキーマを作成し、テスト終了後に削除します。Compose外で`ASTOVIA_TEST_POSTGRES`を指定する場合も、専用テストDBだけを使用してください。未指定の場合、PostgreSQLテストはスキップします。

検証用`compose.test.yaml`のPostgreSQLはtmpfsを使用し、ホストのループバックアドレスの55439番ポートに公開します。本番データや既存のDBは使用しません。ローカルアプリ用の永続SQLiteとは別です。検証後は次で停止できます。

```sh
docker compose -f compose.test.yaml stop postgres
```

## 不具合報告・要望

GitHubのIssueで受け付けます。外部からのプルリクエストは受け付けていません。

## ライセンス

[Apache License 2.0](LICENSE)で提供します。
