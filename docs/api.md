# APIリファレンス

ログインセッション、内蔵のOAuthの認可サーバー、URLと応答、二段階認証をまとめます。

## ログインセッション

- Web画面のログインは、サーバーに保存するセッションと、そのIDを入れたCookie`__Host-astovia_session`（`Secure`・`HttpOnly`・`SameSite=Lax`・`Path=/`）です。トークンを画面のJavaScriptに渡すことはなく、`/api/v1/*`への要求には同じオリジンのCookieが自動で付きます。アクセストークン、リフレッシュ用のCookie、`/auth/refresh`はありません。
- セッションは操作がないまま7日で切れ、操作があってもログインから30日で切れます。期限を延ばすのは`GET`・`HEAD`以外の要求と、画面の読み込み時に呼ぶ`GET /api/v1/me`だけです（画面の定期的な再読み込みでは延びません）。期限切れ・ログアウト済み・無効化されたユーザーのセッションは401`unauthorized`になり、Web画面はログイン画面に戻ります。
- セッションを発行するのは、パスワードでのログイン（第2要素が要らないとき）、第2要素の確認・登録の完了、招待の受諾とパスワード再設定の完了、OIDCのコールバックです。パスワードの再設定、ユーザーの無効化、CLIの`users reset-password`・`users reset-mfa`・`users unlink`は、そのユーザーのセッションをすべて失効させます。
- 以前の版のリフレッシュ用Cookie`astovia_refresh`は、要求に付いていれば`/auth/`配下の応答で消します。
- Cookieで認証する`/api/`と`/auth/`の要求のうち、`GET`・`HEAD`・`OPTIONS`以外は、`Origin`が`ASTOVIA_PUBLIC_URL`と完全に一致しなければ403`origin_rejected`です（`Origin`のない要求も拒否します）。APIトークン（`Authorization: Bearer ast_…`）の要求と、自分で確かめる`POST /auth/oidc/login`は対象外です。CORSのヘッダーは出しません。
- パスワードは12〜1024バイト（UTF-8）で、Argon2idで保存します。`/auth/login`は接続元IPごとに1分で10回まで、加えて正規化したメールアドレスごとに15分で10回失敗すると、そのアドレスのログインを15分止めます（429、`Retry-After: 900`）。存在しないアドレスも同じように数えます。メールアドレスを知っている人は、そのアドレスのログインを15分止められます（受け入れているリスクです。急ぐ場合はプロセスの再起動でも戻ります）。回数はプロセスのメモリに持ち、複数のインスタンスでは共有しません。
- パスワードの照合とハッシュの計算（ログイン、招待の受諾・再設定の完了、ログイン後のパスワードの確認）は、プロセスごとにCPU数の2倍までしか同時に行いません。空きを2秒待っても空かなければ503`unavailable`（`Retry-After: 1`）で、ログインの失敗にも試行の回数にも数えません。

## OAuth

Astoviaは`/mcp`のための認可サーバー（authlumeの`mcpoauth`）を内蔵しています。`ASTOVIA_PUBLIC_URL`が`https`のときだけ有効です。`http`（ローカル開発）では、メタデータ、`/oauth/*`、同意画面`/oauth/authorize`はJSONの404になり、`/mcp`はAPIトークンだけで使えます。`/api/v1/oauth/authorize`は他の`/api/v1`と同じくログインと`Origin`を先に確かめるため、ログインしていなければ401、別のオリジンからの`POST`は403`origin_rejected`で、ログインした同じオリジンの要求だけがJSONの404になります。OAuth 2.1の認可コード方式（PKCE S256必須）とトークンの失効（RFC 7009）に対応し、OpenID Connect、イントロスペクション、JWT形式のトークンは提供しません。

| エンドポイント | 内容 |
| --- | --- |
| `GET /.well-known/oauth-protected-resource/mcp` | 保護リソースメタデータ。`resource`は`<ASTOVIA_PUBLIC_URL>/mcp`、`authorization_servers`は`ASTOVIA_PUBLIC_URL` |
| `GET /.well-known/oauth-protected-resource` | 上と同じ内容。`/mcp`が唯一の保護リソースなので、ルートにフォールバックしたクライアントにも同じリソースを示します（RFC 9728のURL導出とは厳密には一致しません） |
| `GET /.well-known/oauth-authorization-server` | 認可サーバーメタデータ。`issuer`は`ASTOVIA_PUBLIC_URL` |
| `POST /oauth/register` | 動的クライアント登録（DCR） |
| `GET /oauth/authorize` | 認可要求。Web画面の同意画面を返します。画面がクエリをそのまま`/api/v1/oauth/authorize`へ送って要求を確かめます |
| `POST /oauth/token` | `authorization_code`と`refresh_token`グラント |
| `POST /oauth/revoke` | トークンの失効（RFC 7009） |

- スコープは`mcp`（`/mcp`で対応事項を閲覧・作成・更新し、連携中のChatworkとSlackとGmailを読む）と`offline_access`の2つです。`mcp`は必須で、省略すると`mcp`とみなします。`mcp`を含まない要求には`invalid_scope`を返します。リフレッシュトークンは`offline_access`の有無にかかわらず発行します。
- 有効期間: 認可コードは60秒で1回限りです。アクセストークンは既定で1時間です。リフレッシュトークンは使うたびに新しいトークンへ交換（ローテーション）し、最後の交換から既定で30日使われなければ接続が切れます。接続全体の期限は既定ではありません。それぞれ`ASTOVIA_MCP_ACCESS_TTL`（5分〜24時間）、`ASTOVIA_MCP_REFRESH_IDLE`（1時間〜8760時間）、`ASTOVIA_MCP_CONNECTION_ABSOLUTE`（`0`か24時間〜87600時間。`0`は期限なし）で変えられます。値はGoの`time.ParseDuration`の形式で、日の単位はありません（30日は`720h`）。アクセストークンの期限がリフレッシュの期限（と、あれば接続全体の期限）を超える設定は起動時のエラーです。
- 交換済みのリフレッシュトークンか使用済みの認可コードがもう一度使われた場合は、盗まれた可能性があるため、その接続を取り消します（猶予はありません）。
- トークンはランダムな値で、DBにはハッシュだけを保存します。
- クライアントは2通りの方法で識別します。Client ID Metadata Document（CIMD）では、`client_id`がクライアント自身のHTTPSのURLで、Astoviaがその文書を取得して名前とリダイレクトURIを確認します（Claudeが使用）。動的クライアント登録（DCR）では、`/oauth/register`で`client_id`を発行します。どちらも公開クライアントとして扱い、DCRは`client_secret_basic`・`client_secret_post`を求める登録にも`token_endpoint_auth_method: "none"`で応えます（以前の版のクライアントシークレット付きの登録は使えません）。共有シークレットを求めるCIMDの文書は拒否します。事前に登録したクライアントはありません。
- 認可要求には`redirect_uri`が必須です。リダイレクトURIはHTTPSか、ループバック（`localhost`、`127.0.0.1`、`[::1]`）のHTTPに限り、カスタムスキームのURIは登録しても使いません。ループバックのURIだけはポート番号の違いを無視します（Claude Codeは毎回異なるポートで待ち受けます）。
- CIMDの文書は、取得先を名前解決後のIPアドレスで判定し、公開されていないアドレスへは接続しません。タイムアウトは5秒で、リダイレクトとプロキシの環境変数は使いません。`Cache-Control: max-age`に従い、5分〜24時間キャッシュします。
- DCRの登録は接続元IPごとに1分間で10回までです。同意画面でCIMDの取得が要る要求は、ユーザーごとに1分間で10回までです（429`rate_limited`）。どのDCRの登録・接続も指していないCIMDの`client_id`は、`/oauth/token`では取得せずに`invalid_client`で断り、`/oauth/revoke`では200を返します（任意のURLを取得させないためです）。一度も使われないDCRの登録は7日で消えます。
- 登録内容が変わったクライアント（DCRの登録が消えた、CIMDの文書が別の`client_id`を名乗る）は、トークン要求の時点でそのクライアントの接続をすべて取り消します。
- 同意画面は、クライアント名、CIMDなら発行元のホスト（DCRでは名前を確認していない旨）、許可後の戻り先のホスト（ループバックならその旨）、許可する内容、ログイン中のメールアドレスを表示します。同意はWeb画面にログインしているブラウザだけが行えます。未ログインで開いた場合は、ログイン後に同じ同意画面に戻ります。
- 接続中のAIクライアントは管理画面の「接続中の AI クライアント」に一覧表示し、削除するとそのトークンは即座に使えなくなります。パスワードの再設定、ユーザーの無効化、CLIの`users reset-password`・`users reset-mfa`・`users unlink`でも、そのユーザーの接続はすべて削除されます。
- OAuthのアクセストークンは`/mcp`だけで使えます。`/api/v1/*`には使えません。
- トークンは発行時の`ASTOVIA_PUBLIC_URL`に結び付きます。`ASTOVIA_PUBLIC_URL`を変更すると、既存の接続は使えなくなり、AIクライアントから接続し直す必要があります。
- 期限切れの認可コード・接続・DCRの登録の行は、1時間ごとの掃除で削除します。
- `/oauth/*`が返すエラーは`{"error": "...", "error_description": "..."}`の形式です。メソッドの誤り（405）と存在しないURL（404）は、他のURLと同じ`{error: {code, message}}`です。

## URLと応答

| URL | 動作 |
| --- | --- |
| `GET /`、`/candidates`、`/auto`、`/settings`、`/settings/<page>`、`/account`、`/account/<page>`、`/tasks/<id>`、`/oauth/authorize`、`/invite`、`/reset-password`、`/signin-error` | 埋め込みWeb画面（`index.html`）。`/oauth/authorize`は`ASTOVIA_PUBLIC_URL`が`https`のときだけで、`http`ではJSONの404 |
| `GET /assets/*`、`/favicon.ico`等 | 埋め込みWeb画面の静的ファイル（`/sw.js`、`/manifest.webmanifest`を含む）。存在しないファイルは404 |
| `GET /admin/*` | 以前の版のURL。対応する新しいURLへ308でリダイレクト（不明なものは`/`へ）。`/admin/oauth/authorize`は`/`へ。`/admin/sw.js`は古い登録の更新用に`/sw.js`と同じ内容を返す |
| `GET /healthz` | プロセスの稼働確認のみ。非公開情報は返さない |
| `POST /auth/login` | 本文は`{ "email": "alice@example.com", "password": "…" }`（8KiB以内。メールアドレスは256バイト、パスワードは1024バイトを超えると400）。成功すると`{user}`とセッションのCookieを返す。第2要素が要るユーザーにはセッションを発行せず、200`{mfa: …}`を返す（[二段階認証](#二段階認証)）。資格情報の誤り（未登録・無効化・パスワードのないユーザーを含む）は401`unauthorized`、試行回数の上限は429（[ログインセッション](#ログインセッション)） |
| `POST /auth/oidc/login` | OIDCの構成だけ。ログイン画面のフォーム（`application/x-www-form-urlencoded`、16KiB以内）で、項目は任意の`return_to`（ログイン後に戻る画面のパスとクエリ）だけ。接続元IPごとに1分で10回まで（超えると`/signin-error?reason=rate_limited`へ303）、`Origin`が違えば403。`return_to`が8300バイトを超えるか、MCPの同意画面（`/oauth/authorize`）以外で1024バイトを超えれば`/signin-error?reason=return_too_long`へ、画面のパスでなければ`/`に置き換え、IdPの認可エンドポイントへ303。IdPに届かないとき、または途中のログインが全体で10000件あるときは`/signin-error?reason=unavailable`へ303（途中のログインは10分で期限が切れます）。`GET`は405 |
| `GET /auth/oidc/callback` | OIDCの構成だけ。IdPからの戻り先。成功すると`return_to`の画面へ303。失敗は`/signin-error?reason=…`へ303（`failed`・`unavailable`・`disabled`・`not_allowed`・`email_not_verified`・`unsupported_account`）。IdPでの拒否も`failed`になる |
| `POST /auth/logout` | セッションの失効とCookieの削除。本文は`{}`。204 |
| `POST /auth/invitations/inspect` | 本文`{token}`で招待の`{email, role, expires_at}`を返す。無効・期限切れ・受諾済み・取り消し済み・存在しない招待は区別せず404`invitation_invalid` |
| `POST /auth/invitations/accept` | 本文`{token, password}`でユーザーを作成し、Loginと同じ応答とCookieを返す。同じメールアドレスのユーザーがいれば409`email_taken`、パスワードが12〜1024バイトでなければ400。リクエストに付いてきた既存のセッションは置き換える。作成後のログインだけに失敗した場合は503（ログイン画面からログインできる）。第2要素が要るユーザーにはLoginと同じく200`{mfa: …}`を返す |
| `POST /auth/password-resets/inspect` | 本文`{token}`で再設定の`{email}`を返す。無効・期限切れ・使用済み・取り消し済みのリンクと、無効化されたユーザーのリンクは区別せず404`reset_invalid` |
| `POST /auth/password-resets/complete` | 本文`{token, password}`でパスワードを再設定し、Loginと同じ応答とCookieを返す。既存のセッションの扱い・503・`{mfa: …}`の応答はacceptと同じ。二段階認証の設定は残る |
| `POST /auth/mfa/totp` | 本文`{mfa_token, code}`。認証アプリのコードを確かめ、Loginと同じ応答とCookieを返す |
| `POST /auth/mfa/recovery` | 本文`{mfa_token, code}`。リカバリーコードを使用済みにして、Loginと同じ応答とCookieを返す |
| `POST /auth/mfa/webauthn/begin` | 本文`{mfa_token}`。パスキー・セキュリティキーでの確認を始め、200`{options}`を返し、途中の状態のハンドルをCookie`__Host-astovia_webauthn`（5分）に入れる。`options`は`{publicKey}`で、`publicKey`は`PublicKeyCredentialRequestOptions`のJSON形式 |
| `POST /auth/mfa/webauthn/finish` | 本文`{mfa_token, credential}`（32KiB以内）。署名を確かめ、Loginと同じ応答とCookieを返す。同じ応答の再送は401`mfa_code_invalid` |
| `POST /auth/mfa/enroll/totp/begin` | 本文`{mfa_token}`。初回登録の認証アプリの秘密を作り、200`{secret, otpauth_url, setup_token}`を返す |
| `POST /auth/mfa/enroll/totp/finish` | 本文`{mfa_token, setup_token, code}`。認証アプリを登録し、Loginの応答に`recovery_codes`（10個）を足したものとCookieを返す |
| `POST /auth/mfa/enroll/webauthn/begin` | 本文`{mfa_token}`。初回登録のパスキー・セキュリティキーの登録を始め、200`{options}`を返し、途中の状態のハンドルをCookie`__Host-astovia_webauthn`（5分）に入れる。`options`は`{publicKey}`で、`publicKey`は`PublicKeyCredentialCreationOptions`のJSON形式 |
| `POST /auth/mfa/enroll/webauthn/finish` | 本文`{mfa_token, credential, name}`（32KiB以内）。登録し、Loginの応答に`recovery_codes`（10個）を足したものとCookieを返す |
| `GET /api/v1/me` | 本人の`{id, email, role, password_reset}`。ログインセッションが必要で、セッションの無操作の期限を延ばす。Web画面は読み込み時にこれでログイン済みかを確かめる |
| `GET /api/v1/users` | ユーザーの一覧`{users: [{id, email, role, disabled, created_at, mfa_enabled}]}`（作成日時の古い順、1000件まで）。`mfa_enabled`は二段階認証の方法（認証アプリかパスキー・セキュリティキー）が1つ以上あるか。オーナーのログインセッションが必要（メンバーは403`forbidden`、APIトークンは401。以下のユーザー管理APIも同じ） |
| `PATCH /api/v1/users/{id}` | 本文`{role}`か`{disabled}`のどちらか1つで変更し、変更後のユーザーを返す。今と同じ値なら何もせず200。最後の有効なオーナーをメンバーにすると409`last_owner`、自分の無効化は409`self`、存在しないユーザーは404 |
| `POST /api/v1/users/{id}/password-resets` | パスワード再設定リンクを発行し201`{url, expires_at}`を返す（本文なし）。`url`は一度だけ返す。無効化されたユーザーは409`user_disabled` |
| `GET /api/v1/mfa` | 本人の二段階認証`{required, totp, webauthn, recovery_codes_remaining, webauthn_available}`。`totp`は`{created_at}`か`null`、`webauthn`は`{id, name, created_at, last_used_at}`の配列。ログインセッションが必要（APIトークンは401。以下の`/api/v1/mfa/*`も同じ） |
| `POST /api/v1/mfa/totp/begin` | 本文`{password}`で認証アプリの登録を始め、200`{secret, otpauth_url, setup_token}`を返す。登録済みでも受け付ける（入れ替え） |
| `POST /api/v1/mfa/totp/finish` | 本文`{setup_token, code}`で認証アプリを登録し、200`{recovery_codes}`を返す。最初の方法のときだけ10個の配列で、それ以外は`null`。登録済みなら秘密を置き換える |
| `POST /api/v1/mfa/totp/delete` | 本文`{password}`で認証アプリを削除し204を返す。登録がなければ404 |
| `POST /api/v1/mfa/webauthn/begin` | 本文`{password}`でパスキー・セキュリティキーの登録を始め、200`{options}`を返し、ハンドルをCookie`__Host-astovia_webauthn`（5分）に入れる。10個あれば409`mfa_limit` |
| `POST /api/v1/mfa/webauthn/finish` | 本文`{credential, name}`（32KiB以内）で登録し、201`{credential: {id, name, created_at, last_used_at}, recovery_codes}`を返す。`recovery_codes`は最初の方法のときだけ配列で、それ以外は`null` |
| `POST /api/v1/mfa/webauthn/{id}/delete` | 本文`{password}`でパスキー・セキュリティキーを削除し204を返す。他のユーザーのものと存在しないものは404 |
| `POST /api/v1/mfa/recovery-codes` | 本文`{password}`でリカバリーコードを再発行し、200`{recovery_codes}`を返す。前のコードは使えなくなる。方法が1つもなければ409`mfa_not_enrolled` |
| `GET /api/v1/invitations` | 有効な招待の一覧`{invitations: [{id, email, role, created_at, expires_at, invited_by_email}]}`（新しい順、200件まで） |
| `POST /api/v1/invitations` | 本文`{email, role}`で招待を作成し201`{id, email, role, created_at, expires_at, url}`を返す。`url`は一度だけ返す。同じメールアドレスのユーザーがいれば（無効化中も含め）409`email_taken`。OIDCでログインする構成（`ASTOVIA_AUTH_MODE=oidc`）では`url`のない201`{id, email, role, created_at, expires_at}`を返す。招待したメールアドレスの人は、確認済みのメールアドレスが一致する初回のOIDCログインでユーザーとして追加される |
| `DELETE /api/v1/invitations/{id}` | 有効な招待を取り消し204を返す。無い・既に無効な招待は404 |
| `GET /api/v1/action-items` | 本人の対応事項の一覧。クエリは`status`（`open`／`candidate`／`done`／`dismissed`）、`limit`（1〜100、既定50）、`cursor`、`auto`（`true`のみ。自動で処理したもの（自動で完了・対象外にしたもの）だけを処理の新しい順に返す。カーソルは`auto`の有無で別物で、混ぜると400）、`parent_id`（その親の子タスクを作成日時の古い順に100件まで返す。ページングはなく、他のクエリと併用すると400。存在しない・他のユーザーの親は空の一覧）、`view`（`top`のみ。親を持たない対応事項と、親と違う状態の子タスクだけを返す。`status`を省略した場合は親を持たない対応事項だけ。並びとカーソルは`view`なしと同じで、`auto`・`parent_id`と併用すると400）。`{items, next_cursor}`を返す |
| `POST /api/v1/action-items` | 対応事項を作成し201と`Location`を返す。本文は`{content, status?, note?, due_at?, refs?, parent_id?, importance?}` |
| `GET /api/v1/action-items/{id}` | 本人の対応事項を1件返す |
| `PATCH /api/v1/action-items/{id}` | 指定したキー（`content`／`status`／`note`／`due_at`／`refs`／`parent_id`／`importance`）だけを更新する。`note`／`due_at`は`null`で消去、`parent_id`は`null`で親から外す。`version`を付けると一致しない場合に409 |
| `GET /api/v1/api-tokens` | 本人のAPIトークンの一覧`{items: [{id, name, created_at, last_used_at}]}`。トークン本体は含まない。ログインセッションが必要 |
| `POST /api/v1/api-tokens` | APIトークンを発行し201を返す。本文は`{name}`（4KiB以内）。応答の`token`は一度だけ返す。50件に達していると409。ログインセッションが必要 |
| `DELETE /api/v1/api-tokens/{id}` | APIトークンを削除し204を返す。他のユーザーのトークンと存在しないトークンは404。ログインセッションが必要 |
| `GET /api/v1/oauth/authorize?<認可要求のクエリ>` | 同意画面の表示内容。有効な要求は200`{status: "ok", client_name, client_id, client_host, redirect_host, redirect_loopback, scopes, user_id}`（`client_host`はCIMDなら確認済みのホスト、DCRなら`null`。`user_id`はログイン中のユーザー）。クライアントへ返すべき誤り（`mcp`を含まない要求など）は200`{status: "error", error, redirect_url, redirect_host}`で、画面は利用者が押したときだけ`redirect_url`へ移る。戻り先も確かめられない要求は400（`invalid_request`・`unknown_client`・`invalid_client`）、CIMDを取得できなければ503`client_metadata_unavailable`、取得の回数の上限は429`rate_limited`。ログインセッションが必要 |
| `POST /api/v1/oauth/authorize` | 本文`{query, decision, user_id, client_name, client_host}`（`decision`は`approve`か`deny`、16KiB以内）で要求をもう一度確かめて許可・拒否し、移動先`{redirect_url}`を返す。`user_id`・`client_name`・`client_host`は同意画面に表示したユーザーのIDとクライアントの名前・ホスト（GETの`client_name`・`client_host`をそのまま返す。ホストがなければ`null`）。`user_id`がログイン中のユーザーと違えば、クライアントを確かめる前に409`user_changed`（同意画面を開いた後に別のユーザーでログインし直した場合）。確かめ直したクライアントの名前かホストが表示したものと違えば409`client_changed`（メタデータの文書が変わった場合）。どちらも画面を読み込み直す。接続一覧のクライアント名は、許可できたときだけ保存する。エラーはGETと同じ。ログインセッションが必要 |
| `GET /api/v1/oauth/connections` | 接続中のAIクライアントの一覧`{items: [{id, client_name, client_host, scopes, created_at, last_used_at, expires_at}]}`。ログインセッションが必要 |
| `DELETE /api/v1/oauth/connections/{id}` | 接続を削除し204を返す。そのトークンは即座に使えなくなる。他のユーザーの接続と存在しない接続は404。ログインセッションが必要 |
| `GET /api/v1/integrations/github` | GitHub連携の状態。未接続は`{connected: false}`、接続済みは`{connected, login, token_last4, scopes, immediate_notify, status, last_error, last_synced_at, created_at}`。トークン本体は返さない。ログインセッションが必要 |
| `PUT /api/v1/integrations/github` | 本文`{token, immediate_notify?}`（16KiB以内）でトークンを登録・置き換える。無効なトークンは400`github_token_invalid`、`repo`がなければ400`github_scopes_missing`、GitHubに届かなければ503`github_unavailable`。ログインセッションが必要 |
| `PATCH /api/v1/integrations/github` | 本文`{immediate_notify}`だけを受け付ける。未接続は404。ログインセッションが必要 |
| `DELETE /api/v1/integrations/github` | 連携を解除し204を返す。未接続でも204。ログインセッションが必要 |
| `GET /api/v1/integrations/slack` | Slack連携の状態。未接続は`{connected: false}`、接続済みは`{connected, team_name, team_url, user_name, token_last4, scopes, immediate_notify, detect_mentions, detect_dms, detect_group_dms, status, last_error, last_synced_at, created_at}`。トークン本体は返さない。ログインセッションが必要 |
| `PUT /api/v1/integrations/slack` | 本文`{token, immediate_notify?, detect_mentions?, detect_dms?, detect_group_dms?}`（16KiB以内）でUser OAuth Tokenを登録・置き換える。省略した設定は今の値（新規は既定値）のまま。`xoxp-`以外や無効なトークンは400`slack_token_invalid`、権限が足りなければ400`slack_scopes_missing`、Slackに届かなければ503`slack_unavailable`。ログインセッションが必要 |
| `PATCH /api/v1/integrations/slack` | 本文`{immediate_notify?, detect_mentions?, detect_dms?, detect_group_dms?}`（1つ以上）で設定を変える。未接続は404。ログインセッションが必要 |
| `DELETE /api/v1/integrations/slack` | 連携を解除し204を返す。未接続でも204。ログインセッションが必要 |
| `GET /api/v1/integrations/chatwork` | Chatwork連携の状態。未接続は`{connected: false}`、接続済みは`{connected, account_name, organization_name, token_last4, immediate_notify, detect_mentions, detect_dms, import_tasks, status, last_error, last_synced_at, created_at}`。`last_synced_at`は検出とタスクの取り込みのうち新しい方。トークン本体は返さない。ログインセッションが必要 |
| `PUT /api/v1/integrations/chatwork` | 本文`{token, immediate_notify?, detect_mentions?, detect_dms?, import_tasks?}`（16KiB以内）でAPIトークンを登録・置き換える。省略した設定は今の値（新規は既定値）のまま。形式が正しくなければ400`invalid_request`、無効なトークンは400`chatwork_token_invalid`、Chatworkに届かなければ503`chatwork_unavailable`。ログインセッションが必要 |
| `PATCH /api/v1/integrations/chatwork` | 本文`{immediate_notify?, detect_mentions?, detect_dms?, import_tasks?}`（1つ以上）で設定を変える。未接続は404。ログインセッションが必要 |
| `DELETE /api/v1/integrations/chatwork` | 連携を解除し204を返す。未接続でも204。ログインセッションが必要 |
| `GET /api/v1/integrations/gmail` | メール連携の状態`{available, redirect_uri, max_accounts, accounts: [{id, email, immediate_notify, status, last_error, last_synced_at, created_at}]}`。`available`はGoogleのOAuthクライアントが設定されているか。未設定でも接続済みのアカウントは返す。トークンは返さない。ログインセッションが必要 |
| `POST /api/v1/integrations/gmail/authorize` | 本文`{}`（追加）か`{account_id}`（再接続）でGoogleの認可を始め、同意画面のURL`{url}`を返す。未設定は409`gmail_unavailable`、10件に達していれば409`gmail_too_many`（再接続は除く）、他のユーザーのアカウントと存在しないアカウントは404。手続きは10分で無効になる。ログインセッションが必要 |
| `POST /api/v1/integrations/gmail/complete` | 本文`{state, code}`か`{state, error}`でGoogleの認可を完了し、アカウントを返す。形式が正しくなければ400`invalid_request`、手続きが無効・期限切れ・使用済み・他のユーザーのものなら400`gmail_state_invalid`、拒否されたら400`gmail_denied`、`gmail.readonly`がなければ400`gmail_scope_missing`、管理者が許可していなければ403`gmail_policy_blocked`、Gmail APIが無効なら502`gmail_api_disabled`、Googleとのやりとりの失敗は502`gmail_exchange_failed`、別のユーザーが接続済みのアドレスは409`gmail_in_use`、10件に達していれば409`gmail_too_many`。ログインセッションが必要 |
| `PATCH /api/v1/integrations/gmail/accounts/{id}` | 本文`{immediate_notify}`で設定を変える。他のユーザーのアカウントと存在しないアカウントは404。ログインセッションが必要 |
| `DELETE /api/v1/integrations/gmail/accounts/{id}` | アカウントの接続を解除し、Googleでトークンを失効させて204を返す（失効に失敗しても204）。他のユーザーのアカウントと存在しないアカウントは404。ログインセッションが必要 |
| `GET /api/v1/ai` | AIの設定。未設定は`{configured: false}`、設定済みは`{configured, provider, model, key_last4, api_key_set, account, base_url, status, last_error, updated_at}`。`provider`は`"anthropic"`、`"openai"`（ChatGPTのプラン）、`"openai_api"`（OpenAIのAPIキー）、`"openai_compatible"`（OpenAI互換の接続先）のどれか。`key_last4`はAPIキーの末尾4文字（ChatGPTのプランと、キーがないか8文字以下の場合は`""`）、`api_key_set`はAPIキーを保存しているか（ChatGPTのプランとキーのないOpenAI互換の接続先は`false`）、`account`はChatGPTのアカウントのメールアドレス（ほかの提供元と不明な場合は`null`）、`base_url`はOpenAI互換の接続先のURL（ほかの提供元と、保存した設定を読み出せない場合は`null`）。キー・トークンは返さない。ログインセッションが必要 |
| `PUT /api/v1/ai` | 本文`{api_key?, model, provider?, base_url?}`（16KiB以内）でAPIキーとモデルを登録・変更する。`provider`は`"anthropic"`（省略時）、`"openai"`、`"openai_api"`、`"openai_compatible"`のどれかで、今の設定と違えば409`ai_provider_mismatch`（削除してから設定し直す）。Anthropicでは`api_key`を省くと保存済みのキーでモデルだけを変える。保存前にAnthropicで確かめ、無効なキーは400`ai_key_invalid`、モデルを使えなければ400`ai_model_unavailable`、残高・支払いの問題は400`ai_billing`、Anthropicに届かなければ503`ai_unavailable`、別の要求で設定が変わっていれば409`ai_conflict`。`"openai"`はモデルの変更だけで（接続は`/api/v1/ai/openai/*`）、`api_key`は付けられない。モデルがプランの一覧に無ければ400`ai_model_unavailable`、確認の要求が通らなければ、接続が無効なら400`ai_key_invalid`、プランで使えなければ400`ai_billing`、利用上限なら400`ai_usage_limit`、要求の形式を受け付けなければ400`ai_unsupported`、届かなければ503`ai_unavailable`。`"openai_api"`と`"openai_compatible"`では`model`（空白のない印字可能なASCIIで1〜256バイト）が必須。`"openai_api"`では`api_key`を省くか`""`にすると保存済みのキーを使い（未設定なら400`invalid_request`）、`base_url`は付けられない。`"openai_compatible"`では`base_url`（`https`、`ASTOVIA_AI_ALLOW_PRIVATE_ENDPOINTS`なら`http`も可。末尾の`/`は除く）を省くと保存済みのURLを使い、`api_key`は`""`でキーなし、省くと保存済みのキーを使う。URLが保存済みと変わる（または未設定の）ときに`api_key`を省くと400`invalid_request`。ほかの提供元に`base_url`（`null`も含む）を付けると400`invalid_request`。形式の誤りは400`invalid_request`、許可されていない`http`のURLとアドレスは400`ai_endpoint_blocked`、保存済みのキーやURLを読み出せなければ400`ai_key_invalid`。確認の要求は利用者ごとに1分10回までで、超えたら429`rate_limited`。確認の要求が通らなければ、無効なキーは400`ai_key_invalid`、モデル（互換ではURLかモデル）が見つからなければ400`ai_model_unavailable`、残高・支払いの問題は400`ai_billing`、要求の形式（JSON Schemaの構造化出力）を受け付けなければ400`ai_unsupported`、届かなければ503`ai_unavailable`。ログインセッションが必要 |
| `DELETE /api/v1/ai` | AIの設定を削除し204を返す。未設定でも204。ChatGPTのプランではOpenAIでリフレッシュトークンを取り消し、取り消しに失敗したら（削除はしたうえで）200`{revoke_failed: true}`を返す。ログインセッションが必要 |
| `GET /api/v1/ai/instructions` | AIの追加の指示`{instructions: {common, "github.detect", "slack.detect", "chatwork.detect", "gmail.detect", "slack.reply", "chatwork.reply", "gmail.reply"}}`。8つすべてを返し、指示のない欄は`""`。ログインセッションが必要 |
| `PUT /api/v1/ai/instructions` | 本文`{instructions: {…8つすべて}}`（128KiB以内）で追加の指示をまとめて置き換え、`GET`と同じ形を返す。値はすべて文字列で必須。欠落・未知のキー・`null`・文字列以外は400`invalid_request`。各値は前後の空白を除いて保存し、空なら削除する。2000文字を超えたら400`invalid_request`。ログインセッションが必要（APIトークン・MCPでは扱わない） |
| `GET /api/v1/ai/instructions/samples?scope=` | 追加の指示を試せる判定の一覧`{samples: [{id, content, status, auto_reason, conversation_name, detected_at}]}`（最大10件、新しい順）。`scope`は`common`以外の7つで、それ以外は400`invalid_request`。`conversation_name`は最初の参照の会話名か`null`、`detected_at`はRFC 3339か`null`。連携していなければ409`not_connected`。ログインセッションが必要 |
| `POST /api/v1/ai/instructions/try` | 本文`{scope, item_id, common, instruction}`（すべて必須の文字列、128KiB以内）で、判定を保存済みの指示と書きかけの指示（`common`と`instruction`。前後の空白を除いて2000文字以内）でやり直し、検出は`{current: {decision, task, importance, summary, event}, draft: {…}}`（`event`は`{title, start, end}`か`null`。`start`・`end`はAIが返した現地時刻）、返信は`{current: {done}, draft: {done}}`を返す。何も保存しない。不正な本文・`scope`は400`invalid_request`、1分5回を超えたら429`rate_limited`（`Retry-After`付き）、連携していなければ409`not_connected`、試せない判定は404`not_found`、AIの設定が有効でなければ409`ai_not_ready`、利用上限で待っている間は409`ai_cooling_down`、元の会話を読み直せなければ409`source_unavailable`、自分の返信がなければ409`no_reply`、連携先の制限は429`rate_limited`、連携先の失敗は502`upstream_error`、AIの失敗は409`ai_key_invalid`・`ai_model_unavailable`・`ai_billing`・429`ai_usage_limit`・502`ai_error`、55秒を超えたら504`timeout`。ログインセッションが必要 |
| `POST /api/v1/ai/openai/authorize` | 本文`{}`でChatGPTのサインインを始め、`{url, redirect_uri}`を返す。Anthropicの設定があれば409`ai_provider_mismatch`。接続済みならそのクライアントIDで、未接続なら新しいクライアントの登録として始める。手続きは10分で無効になる。ログインセッションが必要 |
| `POST /api/v1/ai/openai/complete` | 本文`{callback_url}`（ブラウザが戻された`http://127.0.0.1:1455/auth/callback?…`のURL。HTTPS-Onlyモードで`https://`になったものも可）で接続を完了し、`GET /api/v1/ai`と同じ形を返す。URLを読み取れなければ400`openai_callback_invalid`（手続きは使われない）、手続きが無効・期限切れ・使用済み・他のユーザーのもの、またはOpenAIがコードを受け付けなければ400`openai_state_invalid`、拒否されたら400`openai_denied`、プランを使う許可がなければ400`openai_scope_missing`、OpenAIとのやりとりやIDトークンの検証に失敗したら502`openai_unavailable`、Anthropicの設定があれば409`ai_provider_mismatch`。モデルの確認の失敗は`PUT`と同じコード。ログインセッションが必要 |
| `GET /api/v1/ai/openai/models` | ChatGPTのプランで使えるモデルの一覧`{models: [{id, name}]}`。ChatGPTのプランを設定していなければ404。エラーは`PUT`と同じコード。ログインセッションが必要 |
| `GET /api/v1/push/public-key` | VAPIDの公開鍵`{public_key}`。ログインセッションが必要 |
| `GET /api/v1/push/subscriptions` | 通知を受け取る端末の一覧`{subscriptions: [{id, label, created_at, last_used_at}]}`。ログインセッションが必要 |
| `POST /api/v1/push/subscriptions` | 本文`{endpoint, keys: {p256dh, auth}, label?, timezone?}`で端末を登録する。新規は201、登録済みの端末は200。20台に達していると409`too_many_subscriptions`。ログインセッションが必要 |
| `DELETE /api/v1/push/subscriptions/{id}` | 端末の登録を削除し204を返す。他のユーザーの端末と存在しない端末は404。ログインセッションが必要 |
| `POST /api/v1/push/test` | Web Pushのテスト通知を送信待ちに入れ202を返す。端末がなければ409`no_subscription`、1分に4回目以降は429（`POST /api/v1/notification-channels/test`と合わせて数える）。ログインセッションが必要 |
| `GET /api/v1/notification-settings` | 通知設定`{timezone, digest_hour, stale_days, candidate_expire_days}`。未保存なら既定値（UTC、8、3、3）。ログインセッションが必要 |
| `PUT /api/v1/notification-settings` | 本文`{timezone, digest_hour, stale_days, candidate_expire_days}`（すべて必須。IANAのタイムゾーン名、0〜23、1〜30、0〜30）で保存する。`candidate_expire_days`はタスク候補を自動で対象外にするまでの日数で、0は対象外にしない。ログインセッションが必要 |
| `GET /api/v1/notification-channels` | 通知の送り先`{routes: {webpush, slack, email}, slack: {configured, last_error}, email: {available, provider, address}}`。`routes`の各値は送る種類（`detected`・`stale`・`due_soon`・`overdue`・`digest`）の配列で、未保存なら`webpush`はすべて、他は空。`slack.last_error`は`webhook_invalid`か`webhook_undecryptable`かnull。メールを設定していないサーバーでは`email.available`がfalseで`provider`と`address`はnull。WebhookのURLは返さない。ログインセッションが必要 |
| `PUT /api/v1/notification-channels` | 本文`{routes: {webpush, slack, email}}`（3つとも必須、16KiB以内）で送り先を保存し、GETと同じ形を返す。値は種類の配列で、重複・知らない種類・nullは400`invalid_request`。保存時に表の順に並べ直す。メールを設定していないサーバーで、保存済みにないメールの種類を足すと400`mail_unavailable`（減らすのは可）。ログインセッションが必要 |
| `PUT /api/v1/notification-channels/slack-webhook` | 本文`{url}`でSlackのIncoming Webhookを登録し（暗号化して保存。送信はしない）、GETと同じ形を返す。`https://hooks.slack.com/services/…/…/…`の形でない（512バイトまで）と400`invalid_request`。ログインセッションが必要 |
| `DELETE /api/v1/notification-channels/slack-webhook` | SlackのWebhookを削除し204を返す（なくても204）。種類の選択は残す。ログインセッションが必要 |
| `POST /api/v1/notification-channels/test` | 本文`{channel}`（`webpush`・`slack`・`email`）の送り先にテスト通知を送信待ちに入れ202`{queued: true}`を返す。メールを設定していないサーバーで`email`は409`mail_unavailable`、SlackのWebhookがない・止まっていると409`no_destination`、`webpush`で端末がなければ409`no_subscription`。`POST /api/v1/push/test`と合わせて1分に4回目以降は429。ログインセッションが必要 |
| `GET /.well-known/oauth-protected-resource/mcp` | MCPの保護リソースメタデータ（RFC 9728）。`/.well-known/oauth-protected-resource`も同じ内容 |
| `GET /.well-known/oauth-authorization-server` | 認可サーバーのメタデータ（RFC 8414） |
| `POST /oauth/register` | クライアント登録（RFC 7591）。`application/json`、16KiB以内。成功は201 |
| `GET /oauth/authorize` | 認可要求。同意画面（`index.html`）を返す |
| `POST /oauth/token` | トークン発行（`authorization_code`／`refresh_token`）。`application/x-www-form-urlencoded`、16KiB以内 |
| `POST /oauth/revoke` | トークンの失効（RFC 7009）。`application/x-www-form-urlencoded`、16KiB以内 |
| `POST /mcp` | MCP（Streamable HTTP、ステートレス）。OAuthのアクセストークンかAPIトークンが必要 |

Login（と第2要素の確認・招待の受諾・パスワード再設定の完了）は、セッションのCookieと`{user: {id, email, role, password_reset}}`を返します（第2要素の初回登録の完了は`recovery_codes`も返します）。`role`は`owner`か`member`、`password_reset`は直近14日以内にパスワード再設定リンクで再設定していれば最新の1件の`{at, by_email}`（発行したオーナーのメールアドレス）、なければ`null`です。

対応事項は`{id, content, status, importance, note, due_at, refs, version, created_at, updated_at, source, auto, kind, event, parent_id, parent, children}`で（`importance`は重要度で`"high"`／`"normal"`／`"low"`。作成時に省略すると`"normal"`。`parent_id`は親タスクのidか`null`、`parent`は親タスクの`{id, content, status}`か`null`、`children`は子タスクがあれば`{total, done, remaining, next_due_at}`（`total`は対象外を除いた子の数、`remaining`は未完了とタスク候補の数、`next_due_at`は未完了とタスク候補の子の一番近い期限か`null`）、なければ`null`。`status`は`open`／`candidate`（AIが見つけた未確定のタスク候補）／`done`／`dismissed`。`source`は連携が作ったものなら`"github"`か`"slack"`、それ以外は`null`。`auto`は自動で完了・対象外にしたものなら`{reason, ref_url, at}`、それ以外は`null`。`kind`は`"task"`か`"event"`（予定タスク）で、`event`は予定タスクなら`{title, start_at, end_at}`（`end_at`は終了時刻がなければ`null`）、それ以外は`null`。`kind`と`event`は作成・更新の本文では受け付けません。MCPの応答も同じ形です）、日時はUTCのRFC 3339です。`/api/v1/action-items*`はすべてログインセッションかAPIトークンが必要で、作成・更新の本文は`application/json`の64KiB以内とします。`content`は1〜500文字（空白のみは不可）、`note`は4000文字以内、`due_at`はオフセット付きのRFC 3339、`refs`は`{service, account?, conversation_id?, conversation_name?, message_id?, url?}`の配列（10件まで、`url`はhttp／https。`conversation_name`は`#general`のような表示用の会話名で、画面ではアカウント名の代わりに表示します）、`parent_id`は自分の対応事項で親を持たないもののid（入れ子は2段までで、子を持つ対応事項には付けられません。違反は400）、`importance`は`"high"`／`"normal"`／`"low"`のいずれか（`null`やそれ以外は400）です。一覧は作成日時の降順で、同じ作成日時の間はidの降順（DBの照合順序に従う）に並びます。続きは`next_cursor`を`cursor`に渡して取得します。他のユーザーの対応事項と存在しない対応事項は、どちらも404になります。削除APIはなく、対象外は`status`の`dismissed`で表します。`status`を変更すると（REST・MCPとも）、その対応事項は連携による自動完了・再オープンの対象から外れ、`auto`は`null`に戻ります。ただし`candidate`と`open`の間の変更（タスク候補をタスクにする、その取り消し）では、Slackの返信による自動完了の対象のままです。開始時刻を過ぎた予定タスクを完了・対象外から`open`か`candidate`に戻すと、`kind`は`"task"`になります。エラーは`{error: {code, message}}`です（`/oauth/*`の処理内で返すエラーだけは、OAuthの形式`{error, error_description}`です）。認証・非公開API・MCP・`/oauth/*`の応答には`Cache-Control: no-store`を付けます。`ASTOVIA_PUBLIC_URL`が`https`でホストがループバック（`localhost`・`*.localhost`・`127.0.0.0/8`・`::1`）でない場合は、すべての応答に`Strict-Transport-Security: max-age=31536000`を付けます（`includeSubDomains`と`preload`は付けません）。存在しないAPI・連携用URLに対して、管理画面のHTMLを返すことはありません。`//`や`..`を含む正規化されていないパスは、メソッドにかかわらず正規化後のパスへ307でリダイレクトし、`%2F`でエスケープしたスラッシュを含むパスはどのURLにも一致せず404になります。

## 二段階認証

パスワードでログインする構成（`ASTOVIA_AUTH_MODE`が`password`）で使えます。OIDCの構成では`/auth/mfa/*`と`/api/v1/mfa*`が409`password_auth_disabled`を返します（`/api/v1/mfa*`は認証が先で、未認証は401）。OIDCの構成ではAstoviaの二段階認証を適用せず（`ASTOVIA_MFA_REQUIRED`も使いません）、IdPの認証を信頼します。多要素認証はIdP側で設定してください。

第2要素はパスキー・セキュリティキー（WebAuthn）と認証アプリ（TOTP。SHA-1・6桁・30秒で、今と1つ前のステップを受け付けます）です。リカバリーコードは第2要素に数えません。`ASTOVIA_MFA_REQUIRED`（`off`・`owners`・`all`）で必須の対象を決めます。

### ログインの応答

`POST /auth/login`・`POST /auth/invitations/accept`・`POST /auth/password-resets/complete`は、第2要素が要るときにセッションを発行しません。セッションのCookieも設定せず、200で次の形を返します。

```json
{"mfa": {"token": "v1.…", "enrolled": true, "methods": {"totp": true, "webauthn": false, "recovery": true}, "webauthn_available": true}}
```

- `enrolled`が`true`なら、`methods`が真の方法のどれかを`/auth/mfa/totp`・`/auth/mfa/webauthn/*`・`/auth/mfa/recovery`で確かめるとセッションが出ます。
- `enrolled`が`false`は、必須の対象でまだ方法を登録していないユーザーです。`methods`はすべて偽で、`/auth/mfa/enroll/*`で登録するとセッションが出ます。
- `webauthn_available`は、このサーバーでパスキー・セキュリティキーを使えるかです。`ASTOVIA_PUBLIC_URL`が`https`のときに真です（`http://localhost`のローカル開発では偽です。ホストがIPアドレスの場合はブラウザーが使わせません）。登録はパスキーとして保存されることがあります（認証器が対応していれば端末に保存する設定です）。パスキーだけでのログインはありません。偽のサーバーでは、WebAuthnのbeginとfinishが400`webauthn_unavailable`を返します。
- `token`（MFAトークン）は`/auth/mfa/*`の本文の`mfa_token`に入れます。期限は`enrolled`が`true`なら10分、`false`なら30分です。使い捨てではありません。パスワードの変更とユーザーの無効化で無効になります。
- OIDCのログインは第2要素を求めません（IdPの認証を信頼します。多要素認証はIdP側で設定してください）。方法の登録・削除・必須化は、既存のセッション・APIトークン・MCPの接続を失効させません。
- パスワードを再設定しても二段階認証は残ります。再設定の完了も上の応答を返します。

### `/auth/mfa/*`（セッションの前）

どれも`POST`で、本文は`application/json`の4KiB以内（WebAuthnのfinishは32KiB以内）です。`Origin`が`ASTOVIA_PUBLIC_URL`と違えば403、`Content-Type`が違えば415です。

- `/auth/mfa/enroll/*`は、方法が1つもなく必須の対象であるユーザーだけが使えます。方法があれば409`mfa_already_enrolled`、必須でなければ403`mfa_not_required`です。
- WebAuthnの`options`は`navigator.credentials.create`・`get`の引数の形`{publicKey: …}`で、`publicKey`の中は仕様のJSON形式です。バイナリ（`challenge`・`user.id`・クレデンシャルのID）はパディングなしのbase64urlです。`credential`には`PublicKeyCredential.toJSON()`と同じ形を送ります。途中の状態はサーバーに保存し、そのハンドルをCookie`__Host-astovia_webauthn`（`Secure`・`HttpOnly`・`SameSite=Strict`・`Path=/`）に入れます。期限は5分で、finishで成否にかかわらず1回で使い切ります。`name`は前後の空白を除いて1〜64文字です。
- `setup_token`の期限は30分です。登録の確認に使ったコードは、直後のログインには使えません。
- リカバリーコードは`xxxx-xxxx-xxxx-xxxx`の形で、1個につき1回だけ使えます。大文字・空白・ハイフンの有無は区別しません。DBにはハッシュだけを保存します。

| エラー | 状況 |
| --- | --- |
| 401`mfa_token_invalid` | MFAトークンが不正・期限切れ、またはユーザーのパスワードか状態が変わった。ログインからやり直す |
| 401`mfa_code_invalid` | コード・署名が正しくない、その方法が登録されていない、使用済みのコード、WebAuthnの応答の再送、WebAuthnの途中の状態が期限切れ・使用済み・別のユーザーのもの |
| 400`mfa_state_invalid` | `setup_token`がない・不正・期限切れ・別のユーザーのもの、WebAuthnのハンドルのCookieがない（長すぎるものも含む） |
| 409`mfa_limit`、409`mfa_credential_exists` | パスキー・セキュリティキーが10個ある、同じものが登録済み |
| 429`rate_limited` | 試行回数の上限。`Retry-After`付き |
| 503`signin_unavailable` | 確認か登録は済んだが、セッションの発行だけに失敗した。使ったコードは戻らず、登録は残る。ログインからやり直す（登録の場合、リカバリーコードはログイン後に再発行する） |
| 503`unavailable` | それ以外の失敗。登録では何も保存していない |

### `/api/v1/mfa*`（ログイン後）

登録の開始・削除・リカバリーコードの再発行は、本文の`password`で現在のパスワードを確かめます（1024バイトまで）。finishは`setup_token`・WebAuthnのハンドルのCookieがその確認を引き継ぎます。

資格情報の誤りに401を使いません（Web画面は`/api/v1/*`の401をセッションの終わりとして扱い、ログイン画面に戻るためです）。

| エラー | 状況 |
| --- | --- |
| 400`password_invalid` | 現在のパスワードが違う |
| 400`mfa_code_invalid` | 認証アプリのコード・WebAuthnの応答が正しくない、WebAuthnの途中の状態が期限切れ・使用済み・別のユーザーのもの |
| 400`mfa_state_invalid` | `setup_token`がない・不正・期限切れ・別のユーザーのもの、WebAuthnのハンドルのCookieがない（長すぎるものも含む） |
| 400`webauthn_unavailable` | このサーバーではパスキー・セキュリティキーを使えない（削除はできる） |
| 409`mfa_required` | 必須の対象のユーザーが最後の方法を削除しようとした |
| 409`mfa_limit`、409`mfa_credential_exists`、409`mfa_not_enrolled` | 10個ある、同じものが登録済み、方法がないのにコードを再発行しようとした |
| 429`rate_limited` | 試行回数の上限。`Retry-After`付き |

最後の方法を削除すると、リカバリーコードも削除します。

### 試行回数と受け入れているリスク

- 第2要素の確認（`/auth/mfa/totp`・`/auth/mfa/recovery`・`/auth/mfa/webauthn/finish`・`/auth/mfa/enroll/totp/finish`・`/api/v1/mfa/totp/finish`）は、ユーザーごとに15分で5回までです（`Retry-After: 900`）。成功した試行も数えます。
- 現在のパスワードの確認は、ユーザーごとに5分で10回までです（`Retry-After: 300`）。
- `/auth/mfa/*`全体は、接続元IPごとに1分で10回までです（`Retry-After: 60`）。`ASTOVIA_TRUSTED_PROXIES`で指定した前段から届いた要求は、`X-Forwarded-For`の右から見て最初の信頼しないアドレスで数えます。指定がない場合、リバースプロキシ配下では全員がプロキシのIPとしてまとめて数えられます。
- 回数はプロセスのメモリに持ちます。再起動で消え、複数のインスタンスでは共有しません。
- パスワードを知っている相手は、認証アプリのコードを15分に5回試せます。1回あたり約100万分の3で当たるので、試し続けられると1日で約0.14%、30日で約4%の確率で通ります。失敗が続いたら止める仕組みはありません。ログの`auth: mfa: totp failed user=<id>`が続いていたら、そのユーザーのパスワードを変えてください。
- 同じ相手が上限まで失敗させ続けている間は、本人も第2要素を通れません。この場合の対処もパスワードの変更です。
- ログにはユーザーIDだけを出します。失敗は`auth: mfa: <totp|recovery|webauthn> failed user=<id>`、登録・削除・再発行は`auth: mfa: <内容> user=<id>`です。コード・トークン・秘密は出しません。

### 解除と復旧

- すべての方法とリカバリーコードを失くしたユーザーの解除は、CLIの`astovia users reset-mfa --email ADDRESS`だけです。オーナーの画面とAPIからはできません。`reset-mfa`は、そのユーザーのセッション・APIトークン・MCPの接続もすべて失効させます。解除後、必須の対象のユーザーは次のパスワードログインで登録し直します。それ以外のユーザーはパスワードだけでログインできます。
- 方法を登録していないユーザーのパスワードを知った相手は、必須の構成では自分の方法を登録できます。この場合、パスワードを再設定しても相手の方法とリカバリーコードが残ります。復旧には`users reset-password`と`users reset-mfa`の両方が必要です。
- `ASTOVIA_JWT_SECRET`を変えると認証アプリのコードが使えなくなります。パスキー・セキュリティキーとリカバリーコードは使えます。該当するユーザーは`users reset-mfa`で解除して登録し直します。
- 認証の置き換え（authlume）より前の版で登録した認証アプリとパスキー・セキュリティキーは使えません。行は残り、そのユーザーには第2要素が必須のままです（黙ってパスワードだけに戻しません）。リカバリーコードは引き続き使えます。リカバリーコードでログインして登録し直すか、オーナーが`users reset-mfa`で解除します。使えない認証アプリの行は新しい登録で置き換わり、使えないパスキー・セキュリティキーの行は10個の上限に数えません。
