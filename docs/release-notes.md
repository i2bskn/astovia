# リリースノート

## 次の版: 認証をauthlumeに置き換え

ログイン、二段階認証、OIDCのログイン、MCPのOAuthを、ライブラリのauthlumeで作り直しました。更新するとデータの一部が使えなくなるため、更新の前に以下を読んでください。

### 更新の手順

1. 更新の前に、DBのバックアップを取ります。
2. スキーマの変更計画を確かめます。Atlasは置き換えた認証のテーブル（`sessions`・`refresh_tokens`・`oauth_clients`・`oauth_grants`・`oauth_tokens`・`mfa_used_challenges`）の削除と、新しいテーブル（`web_sessions`・`oidc_logins`・`webauthn_ceremonies`・`mcp_oauth_codes`・`mcp_oauth_connections`・`mcp_oauth_registrations`・`mcp_oauth_client_labels`）と列（`users.oidc_link_approved_until`、`mfa_totp.id`、`mfa_webauthn_credentials`の`version`・`passkey`など）の追加を計画します。SQLite・PostgreSQLとも、`atlas schema apply --dry-run`で、これ以外のテーブルの削除や作り直しがないことを確かめてください。
3. スキーマを適用します（配布構成では`docker compose run --rm migrate`）。旧テーブルの削除は承認して構いません。追加した列には既定値があるため、古い行が残っていても適用できます。
4. 構成に合わせて、下の「パスワードでログインする構成」か「OIDCでログインする構成」の手順を行います。
5. 配布構成では`deploy/compose.yaml`も更新します。新しい環境変数を渡す行はこの版の`compose.yaml`にあり、以前の`compose.yaml`では`.env`の値がアプリに届きません。

### 全員に影響すること

- 全員がログインし直します。ログインはサーバーに保存するセッションとCookie`__Host-astovia_session`（操作がないまま7日、ログインから30日で切れます）になり、アクセストークン、リフレッシュ用のCookie、`/auth/refresh`はなくなりました。以前のCookie`astovia_refresh`はサーバーが消します。
- MCPのクライアント（Claude、Claude Code、ChatGPTなど）は、すべてAstoviaへ接続し直します。以前の接続とクライアントの登録は旧テーブルとともに消えます。クライアント側でAstoviaのサーバーをいったん削除して追加し直してください。
- APIトークン、連携の設定、対応事項などのデータはそのまま使えます。
- パスキー・セキュリティキーを新しく登録すると、パスキーとして端末に保存されることがあります（認証器が対応していれば端末に保存する設定になったためです）。パスキーだけでのログインはありません。

### パスワードでログインする構成

- パスワードのハッシュは移行しません。新しい版はArgon2idのハッシュだけを受け付けるため、以前のパスワードではログインできません。
  1. オーナーは自分のパスワードをCLIで設定し直します: `docker compose run --rm app users reset-password --email you@example.com`
  2. ほかのユーザーには、ユーザー管理画面からパスワード再設定リンクを発行して渡すか、同じCLIで設定し直します。
- パスワードの長さは12〜1024バイトになりました（以前の上限は72バイト）。
- 二段階認証の認証アプリ・パスキー・セキュリティキーは登録し直します。以前の版で登録したものは使えません。行は残り、そのユーザーには第2要素が必須のままです（黙ってパスワードだけに戻しません）。
  - リカバリーコードが残っているユーザーは、リカバリーコードでログインし、アカウントの画面で登録し直せます。
  - 第2要素が認証アプリ・パスキー・セキュリティキーだけでリカバリーコードがないユーザーは、ログインできません。オーナーが`docker compose run --rm app users reset-mfa --email someone@example.com`で解除してください。`ASTOVIA_MFA_REQUIRED`の対象なら次のログインで登録し直し、それ以外ならパスワードだけでログインできるようになります。
- `users reset-mfa`は、そのユーザーのセッション・APIトークン・MCPの接続もすべて失効させるようになりました。
- 新しい制限: 正規化したメールアドレスごとに、15分で10回ログインに失敗すると、そのアドレスのログインを15分止めます（429）。存在しないアドレスも同じように数えます。メールアドレスを知っている人は、そのアドレスのログインを15分止められます。これは受け入れているリスクです。止まっても15分待てば戻り、急ぐ場合はプロセスの再起動でも戻ります。回数はプロセスのメモリに持ち、複数のインスタンスでは共有しません。

### OIDCでログインする構成

- OIDCのログインは`ASTOVIA_PUBLIC_URL`が`https`のときだけ使えます（`http`では起動時のエラーです）。
- メールアドレスが同じだけでは、既存のユーザーに紐付かなくなりました。紐付けのないユーザーは、オーナーの承認があるときだけ、確認済みのメールアドレスが一致する初回のログインで紐付きます。承認は7日で切れ、紐付けに成功するかユーザーを無効化すると消えます。画面からは承認できません。
  - `users create --no-password`は、作成と同時に7日間承認します。
  - `users approve-oidc --email ADDRESS`（新設）で、紐付けのないユーザーを7日間承認します。期限が切れた場合や、パスワードでログインしていたユーザーをOIDCに移す場合に使います。承認のないユーザーのログインは`not_allowed`で断ります。
  - `users unlink`は紐付けを外し、そのユーザーのセッション・APIトークン・MCPの接続をすべて失効させます。紐付け直す承認は付けないため、続けて`users approve-oidc`を実行してください。
- 招待からのユーザーの作成は今までどおりです。
- AstoviaはIdPの認証を信頼し、OIDCの構成ではAstoviaの二段階認証を適用しません（`ASTOVIA_MFA_REQUIRED`も使いません）。多要素認証はIdP側で設定してください。
- ログインのボタンは、Astoviaへフォームを送り、AstoviaがIdPへ転送する形になりました。ブラウザーはフォームの後の転送すべてにContent-Security-Policyの`form-action`を当てます。`ASTOVIA_OIDC_ISSUER`のオリジンは常に許可します。IdPの認可エンドポイントがissuerと別のオリジンの場合や、IdPがさらに別のオリジンへ転送する場合（Cloudflare Accessから上流のIdP、Google Workspaceから外部のSAMLなど）は、その先をすべて`ASTOVIA_OIDC_FORM_ACTION_ORIGINS`（新設、空白区切りのhttpsのオリジン）に並べてください。ボタンを押した後にブラウザーで止まる場合は、ブラウザーのコンソールに止めたオリジンが出ます。
- ログインの開始は接続元IPごとに1分で10回までです（超えると`/signin-error?reason=rate_limited`）。
- IdPでログインを拒否した場合も、`failed`（ログインできませんでした）として表示します。以前の`denied`はなくなりました。

### MCPのOAuth

- アクセストークンは既定で1時間です。リフレッシュトークンは`offline_access`の有無にかかわらず発行し、最後の更新から既定で30日使われなければ接続が切れます。接続全体の期限は既定でなくなりました（以前は同意から180日）。以前の「`offline_access`がなければ7日のアクセストークンだけ」はなくなりました。
- 新しい環境変数`ASTOVIA_MCP_ACCESS_TTL`（5分〜24時間）、`ASTOVIA_MCP_REFRESH_IDLE`（1時間〜8760時間）、`ASTOVIA_MCP_CONNECTION_ABSOLUTE`（`0`か24時間〜87600時間）で期限を変えられます。値はGoの`time.ParseDuration`の形式（30日は`720h`）です。
- `offline_access`を要求しないクライアント（ChatGPTなど）が、1時間ごとにリフレッシュトークンで更新できるかは、実際のクライアントで確かめていません。接続が1時間ほどで切れる場合はお知らせください。
- 動的クライアント登録（DCR）の登録は、すべて公開クライアント（`token_endpoint_auth_method: "none"`）になりました。クライアントシークレットを求める登録にも`none`で応えます。
- トークンの失効のエンドポイント`/oauth/revoke`（RFC 7009）を追加しました。
- 同意画面のURLは`/oauth/authorize`になりました（以前の`/consent`はなくなり、`/admin/oauth/authorize`は`/`へ転送します）。
- `ASTOVIA_PUBLIC_URL`が`http`の構成（ローカル開発）では、MCPのOAuthは使えません。`/mcp`はAPIトークンだけで使えます。

### ローカル開発（`http://localhost`）

- パスワードのログインとセッションは使えます。ブラウザーは`localhost`を安全な文脈として扱い、`Secure`のCookieを保存します（ChromeとFirefoxを想定しています。Safariでは確認していません）。
- パスキー・セキュリティキー、MCPのOAuth、OIDCのログインは使えません（authlumeがhttpsを求めるためです）。

### 確かめていないこと

- `offline_access`を要求しないクライアント（ChatGPTなど）の接続の更新。
- 実際のIdP（Google、Cloudflare Accessとその先のIdP）での、フォームの送信と`form-action`。
- Safariでの`http://localhost`の`Secure`のCookie。
