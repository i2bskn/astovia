// Screens of the dashboard. React Router serves them under the /admin
// basename; path is relative to it.
export type View = 'tasks' | 'auto' | 'settings';

export const basename = '/admin';

export const views: readonly { value: View; label: string; path: string }[] = [
  { value: 'tasks', label: 'タスク', path: '/' },
  { value: 'auto', label: '自動で処理したもの', path: '/auto' },
  { value: 'settings', label: '設定', path: '/settings' },
];

// viewURL returns the absolute path of a view, for links made outside the
// router such as notifications.
export function viewURL(view: View): string {
  const path = views.find((v) => v.value === view)?.path ?? '/';
  return path === '/' ? `${basename}/` : basename + path;
}
