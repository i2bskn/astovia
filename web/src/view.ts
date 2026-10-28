// Screens of the web UI. React Router serves them at the root; path is the
// absolute path of the screen.
export type View = 'tasks' | 'candidates' | 'auto' | 'settings';

// short is the label under an icon in the narrow menus.
export const views: readonly { value: View; label: string; short: string; path: string }[] = [
  { value: 'tasks', label: 'タスク', short: 'タスク', path: '/' },
  { value: 'candidates', label: 'タスク候補', short: '候補', path: '/candidates' },
  { value: 'auto', label: '自動で処理したもの', short: '自動処理', path: '/auto' },
  { value: 'settings', label: '設定', short: '設定', path: '/settings' },
];

// viewURL returns the absolute path of a view, for links made outside the
// router such as notifications.
export function viewURL(view: View): string {
  return views.find((v) => v.value === view)?.path ?? '/';
}

// countLabel is the count shown for a list read one page at a time: n, or
// n+ when there are further pages.
export function countLabel(n: number, more: boolean): string {
  return more ? `${n}+` : String(n);
}
