# APIリファレンス

内蔵のOAuthの認可サーバーと、URLと応答をまとめます。

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
