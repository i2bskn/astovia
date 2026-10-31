# Astovia

Astoviaは、「忘れてた！」を防ぐためのセルフホストのタスク管理です。Web画面でタスクを管理し、スマホなどへWeb Pushで通知します。

自分へのメンションなどは、AI（AnthropicのAPIキー、自分のChatGPTのプラン、OpenAIのAPIキー、OpenAI互換の接続先のいずれか）が読んでやることがあるものをタスクにし、自分宛てか判断しきれないものは「タスク候補」にします。

- GitHub連携: レビュー依頼と割り当てを取り込んで、PR／Issueの状態から自動で完了にします。自分へのメンションは、AIが読んでタスクとタスク候補にします。
- Slack連携: 自分へのメンションとDM（設定でグループDMも）をAIが読んで同じようにタスクとタスク候補にし、自分の返信で用件が済んだとAIが判断したら自動で完了にします。
- Chatwork連携: 自分宛てのTO・返信・TO ALLとダイレクトチャットを同じようにAIが読んでタスクにし、自分に割り当てられたChatworkのタスクも取り込みます。
- メール連携（Gmail・Google Workspace）: 受信トレイに届いたメールをスレッドごとにAIが読んで同じようにタスクにし、自分の返信で用件が済んだら自動で完了にします。
- 予定タスク: 会議などの日時が決まった投稿は、Googleカレンダーに登録するタスク（予定タスク）にします。
- REST APIとMCP: AIクライアントや外部プログラムからは、REST APIとMCP（OAuthで接続）で操作できます。
- 保存先: SQLiteかPostgreSQLです。

GitHub・Slack・Chatwork・メール（Gmail）以外の外部サービス連携は**まだ実装していません**。

## 必要なもの

- HTTPSで公開してください。Web PushとCookieの`Secure`属性にHTTPSが必要です。
- 同じDBに対して動かすプロセスは1つだけにしてください。
- `ASTOVIA_JWT_SECRET`は`openssl rand -base64 32`で一度だけ生成し、保管してください。変更すると、保存した連携のトークンやAIの資格情報を復号できなくなります。
- AIの接続先として、AnthropicのAPIキー、ChatGPTのプラン、OpenAIのAPIキー、OpenAI互換の接続先のいずれかが必要です。

## 試す（VPS）

Docker Composeが動き、80番と443番のポートを外部から受け付けるサーバーと、そのサーバーを指すドメインのA／AAAAレコードが必要です。

1. 使うバージョンのタグから設定ファイルを取得します（`vX.Y.Z`は使うバージョンに置き換えてください）。

   ```sh
   mkdir astovia && cd astovia
   for f in compose.yaml Caddyfile .env.example; do
     curl -fsSLO "https://raw.githubusercontent.com/i2bskn/astovia/vX.Y.Z/deploy/$f"
   done
   ```

2. `.env`を作成し、`ASTOVIA_VERSION`・`ASTOVIA_DOMAIN`・`ASTOVIA_JWT_SECRET`を設定します。

   ```sh
   cp .env.example .env
   chmod 600 .env
   openssl rand -base64 32   # ASTOVIA_JWT_SECRETに設定する値
   ```

3. スキーマを適用します。Atlasが表示する変更計画を確認してから承認してください。

   ```sh
   docker compose run --rm migrate
   ```

4. 初期ユーザー（オーナー）を作成します。

   ```sh
   docker compose run --rm app users create --email you@example.com
   ```

5. 起動し、`https://<ASTOVIA_DOMAIN>/`を開いてログインします。

   ```sh
   docker compose up -d
   ```

詳しい手順と、バックアップ・復元・更新の手順は[VPSで動かす（SQLite＋Caddy）](https://astovia.dev/guides/self-hosting/)を参照してください。

## ドキュメント

### 導入

- [導入の前に](https://astovia.dev/guides/requirements/)
- [VPSで動かす（SQLite＋Caddy）](https://astovia.dev/guides/self-hosting/)
- [コンテナサービスと外部PostgreSQLで動かす](https://astovia.dev/guides/container-postgres/)
- [ネイティブバイナリで動かす](https://astovia.dev/guides/native/)

### 運用

- [ユーザーを管理する](https://astovia.dev/guides/users/)
- [ログイン方式と認証](https://astovia.dev/guides/login/)

### 連携

- [GitHub連携](https://astovia.dev/guides/github/)
- [Slack連携](https://astovia.dev/guides/slack/)
- [Chatwork連携](https://astovia.dev/guides/chatwork/)
- [メール連携（Gmail・Google Workspace）](https://astovia.dev/guides/gmail/)

### 機能

- [通知](https://astovia.dev/guides/notifications/)
- [タスク候補・予定タスク・入れ子](https://astovia.dev/guides/tasks/)
- [AIの設定](https://astovia.dev/guides/ai/)
- [MCPとAPIトークン](https://astovia.dev/guides/mcp/)

## 開発・API

- [開発](docs/development.md): Dockerでローカル開発する、コード生成と検証、リリース
- [APIリファレンス](docs/api.md): OAuth、URLと応答

## 不具合報告・要望

GitHubのIssueで受け付けます。外部からのプルリクエストは受け付けていません。

## ライセンス

[Apache License 2.0](LICENSE)で提供します。
