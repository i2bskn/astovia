// The dashboard has no router: the view query parameter of /admin/ selects
// the screen, and history.pushState / popstate move between them.
export type View = 'tasks' | 'auto' | 'settings';

export const views: readonly { value: View; label: string }[] = [
  { value: 'tasks', label: 'タスク' },
  { value: 'auto', label: '自動で処理したもの' },
  { value: 'settings', label: '設定' },
];

// parseView reads the view from a location.search value. Unknown or missing
// values show the tasks.
export function parseView(search: string): View {
  const value = new URLSearchParams(search).get('view');
  return value === 'auto' || value === 'settings' ? value : 'tasks';
}

// viewPath returns the /admin/ URL of a view.
export function viewPath(view: View): string {
  return view === 'tasks' ? '/admin/' : `/admin/?view=${view}`;
}
