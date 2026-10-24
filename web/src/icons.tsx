import type { ReactNode } from 'react';

// Stroke icons on a 24px grid. They are decorative: the control that holds
// one carries the accessible name.
function Icon({ children, size = 20 }: { children: ReactNode; size?: number }) {
  return <svg className="icon" width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">{children}</svg>;
}

type IconProps = { size?: number };

export const TasksIcon = ({ size }: IconProps) => <Icon size={size}><path d="M4 6.5l1.6 1.6L8.5 5" /><path d="M4 12.5l1.6 1.6 2.9-3.1" /><path d="M4.5 18.5h3" /><path d="M11.5 6.5H20M11.5 12.5H20M11.5 18.5H20" /></Icon>;
export const AutoIcon = ({ size }: IconProps) => <Icon size={size}><path d="M20 12a8 8 0 1 1-2.6-5.9" /><path d="M20 4v4h-4" /><path d="M8.5 12.2l2.4 2.4 4.6-4.8" /></Icon>;
export const SettingsIcon = ({ size }: IconProps) => <Icon size={size}><path d="M4 7h9M17 7h3M4 17h3M11 17h9" /><circle cx="15" cy="7" r="2" /><circle cx="9" cy="17" r="2" /></Icon>;
export const SidebarIcon = ({ size }: IconProps) => <Icon size={size}><rect x="3.5" y="4.5" width="17" height="15" rx="3" /><path d="M9.5 4.5v15" /></Icon>;
export const ExternalIcon = ({ size = 14 }: IconProps) => <Icon size={size}><path d="M14 5h5v5" /><path d="M19 5l-8 8" /><path d="M17 14v4a1.5 1.5 0 0 1-1.5 1.5h-9A1.5 1.5 0 0 1 5 18V8.5A1.5 1.5 0 0 1 6.5 7H10" /></Icon>;
export const CheckIcon = ({ size = 14 }: IconProps) => <Icon size={size}><path d="M5 12.5l4.2 4.2L19 7" /></Icon>;
export const ClockIcon = ({ size = 14 }: IconProps) => <Icon size={size}><circle cx="12" cy="12" r="8" /><path d="M12 7.5V12l3 2" /></Icon>;
export const PlusIcon = ({ size = 18 }: IconProps) => <Icon size={size}><path d="M12 5v14M5 12h14" /></Icon>;
export const UndoIcon = ({ size = 16 }: IconProps) => <Icon size={size}><path d="M9 14L4 9l5-5" /><path d="M4 9h10a6 6 0 0 1 0 12h-3" /></Icon>;
export const DismissIcon = ({ size = 16 }: IconProps) => <Icon size={size}><circle cx="12" cy="12" r="8" /><path d="M8.5 12h7" /></Icon>;
export const BellIcon = ({ size }: IconProps) => <Icon size={size}><path d="M6 16.5V11a6 6 0 0 1 12 0v5.5l1.5 1.5h-15z" /><path d="M10 20.5a2.2 2.2 0 0 0 4 0" /></Icon>;
export const PlugIcon = ({ size }: IconProps) => <Icon size={size}><path d="M9 3.5V8M15 3.5V8" /><path d="M6.5 8h11v3a5.5 5.5 0 0 1-11 0z" /><path d="M12 16.5v4" /></Icon>;
export const KeyIcon = ({ size }: IconProps) => <Icon size={size}><circle cx="8" cy="15" r="4" /><path d="M11 12l8.5-8.5M16 7l2.5 2.5M14 9l2 2" /></Icon>;
export const UserIcon = ({ size }: IconProps) => <Icon size={size}><circle cx="12" cy="8.5" r="3.5" /><path d="M5 20a7 7 0 0 1 14 0" /></Icon>;
export const SourceIcon = ({ size = 14 }: IconProps) => <Icon size={size}><circle cx="6" cy="6" r="2" /><circle cx="6" cy="18" r="2" /><circle cx="18" cy="8" r="2" /><path d="M6 8v8M18 10a6 6 0 0 1-6 6H8" /></Icon>;
