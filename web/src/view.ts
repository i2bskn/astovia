// Screens of the dashboard. React Router serves them under the /admin
// basename; path is relative to it.
export type View = 'tasks' | 'auto' | 'settings';

export const basename = '/admin';

// short is the label under an icon in the narrow menus.
export const views: readonly { value: View; label: string; short: string; path: string }[] = [
  { value: 'tasks', label: 'タスク', short: 'タスク', path: '/' },
  { value: 'auto', label: '自動で処理したもの', short: '自動処理', path: '/auto' },
  { value: 'settings', label: '設定', short: '設定', path: '/settings' },
];

// viewURL returns the absolute path of a view, for links made outside the
// router such as notifications.
export function viewURL(view: View): string {
  const path = views.find((v) => v.value === view)?.path ?? '/';
  return path === '/' ? `${basename}/` : basename + path;
}
