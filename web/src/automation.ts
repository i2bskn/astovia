// Labels and checks for automatically processed items and the GitHub
// integration. Pure functions, tested without a browser.

const reasonLabels: Readonly<Record<string, string>> = {
  'github.review_submitted': 'レビューを提出した',
  'github.review_request_removed': 'レビュー依頼が取り消された',
  'github.pr_merged': 'PR がマージされた',
  'github.pr_closed': 'PR がクローズされた',
  'github.issue_closed': 'Issue がクローズされた',
  'github.unassigned': '割り当てが外れた',
  'github.mention_no_action': '対応は不要と判断した',
};

// autoReasonLabel returns the Japanese label of an automatic completion
// reason, or the code itself when it is unknown.
export function autoReasonLabel(code: string): string {
  return Object.hasOwn(reasonLabels, code) ? reasonLabels[code] ?? code : code;
}

// httpsURL returns the URL when it is a valid https URL, so that only such
// URLs become links.
export function httpsURL(value: string | null | undefined): string | null {
  if (!value) return null;
  try {
    const url = new URL(value);
    return url.protocol === 'https:' ? url.href : null;
  } catch {
    return null;
  }
}

const connectionErrors: Readonly<Record<string, string>> = {
  token_invalid: 'トークンが無効になりました。トークンを更新してください。',
  token_undecryptable: '保存したトークンを読み出せません。トークンを登録し直してください。',
  rate_limited: 'GitHub の利用上限に達したため、一時的に止まっています。',
  unavailable: 'GitHub に接続できない状態が続いています。',
};

// githubStatusMessage describes the synchronization state of a connection.
export function githubStatusMessage(status: string, lastError: string | null): string {
  if (lastError && Object.hasOwn(connectionErrors, lastError)) return connectionErrors[lastError] ?? lastError;
  if (status === 'error') return 'エラーで停止しています。トークンを更新してください。';
  return '正常';
}

// githubErrorMessage describes why registering a token failed, from the
// error code of the response.
export function githubErrorMessage(code: string | null): string {
  switch (code) {
    case 'github_token_invalid':
      return 'トークンが無効です。';
    case 'github_scopes_missing':
      return '必要な権限（repo）が足りません。';
    case 'github_unavailable':
      return 'GitHub に接続できませんでした。時間をおいて試してください。';
    case 'invalid_request':
      return 'トークンの形式が正しくありません。';
  }
  return 'GitHub と接続できませんでした。接続を確認して、もう一度お試しください。';
}

// GITHUB_TOKEN_URL opens GitHub's classic token page with the scopes Astovia
// needs.
export const GITHUB_TOKEN_URL = 'https://github.com/settings/tokens/new?scopes=repo,notifications&description=Astovia';
