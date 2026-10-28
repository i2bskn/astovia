// The Slack app manifest shown in the settings, the same text as
// deploy/slack-app-manifest.yaml (a test checks that they match). The app
// only reads, with a user token.
export const SLACK_APP_MANIFEST = `display_information:
  name: Astovia
  description: Reads your mentions and direct messages so that Astovia can turn them into tasks.
oauth_config:
  scopes:
    user:
      - search:read
      - users:read
      - channels:read
      - groups:read
      - im:read
      - mpim:read
      - channels:history
      - groups:history
      - im:history
      - mpim:history
settings:
  org_deploy_enabled: false
  socket_mode_enabled: false
  token_rotation_enabled: false
`;

// SLACK_APP_URL opens Slack's page to create an app.
export const SLACK_APP_URL = 'https://api.slack.com/apps?new_app=1';
