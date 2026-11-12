import { useEffect, type ReactNode } from 'react';
import { ArrowLeft, Boxes, FolderOpen, KeyRound, RefreshCw, Settings2, ShieldCheck, UserRound, UsersRound } from 'lucide-react';
import './AdminWorkspace.css';

const sections = [
  { id: 'catalog', title: 'Пространства', icon: FolderOpen },
  { id: 'teams', title: 'Команды', icon: UsersRound, admin: true },
  { id: 'projects', title: 'Проекты', icon: Boxes },
  { id: 'users', title: 'Пользователи', icon: UserRound, admin: true },
  { id: 'tokens', title: 'MCP-доступ', icon: KeyRound, corporate: true },
  { id: 'settings', title: 'Настройки', icon: Settings2, admin: true },
] as const;

export type AdminSection = typeof sections[number]['id'];

type Props = {
  active: AdminSection;
  onNavigate: (section: AdminSection) => void;
  onClose: () => void;
  isAdmin: boolean;
  personalMode: boolean;
  busy: boolean;
  error: string;
  onRefresh?: () => void;
  children: ReactNode;
};

export function AdminWorkspace({ active, onNavigate, onClose, isAdmin, personalMode, busy, error, onRefresh, children }: Props) {
  const activeSection = sections.find(section => section.id === active);
  useEffect(() => {
    document.title = `${activeSection?.title ?? 'Управление'} — AI Wiki`;
    window.scrollTo(0, 0);
  }, [activeSection]);
  return <div className="corp-admin-page">
    <aside className="admin-sidebar">
      <div className="admin-sidebar-title"><ShieldCheck size={20}/><span>Управление AI Wiki</span></div>
      <nav aria-label="Разделы управления">
        {sections.filter(section => (!('admin' in section) || isAdmin) && (!('corporate' in section) || !personalMode)).map(({ id, title, icon: Icon }) =>
          <button key={id} type="button" onClick={() => onNavigate(id)} aria-current={id === active ? 'page' : undefined} title={title}>
            <Icon size={18} aria-hidden="true"/><span>{title}</span>
          </button>)}
      </nav>
      <button className="admin-sidebar-return" type="button" onClick={onClose} title="К базе знаний">
        <ArrowLeft size={18} aria-hidden="true"/><span>К базе знаний</span>
      </button>
    </aside>
    <section className="corp-admin" aria-labelledby="corp-admin-title">
      <header className="admin-page-heading">
        <h1 id="corp-admin-title">{activeSection?.title}</h1>
        <div className="admin-heading-actions">
          <button className="button ghost" type="button" onClick={onClose}><ArrowLeft size={15} aria-hidden="true"/>К базе знаний</button>
          {onRefresh && <button className="button ghost" type="button" onClick={onRefresh} disabled={busy}>
            <RefreshCw size={15} aria-hidden="true"/>Обновить
          </button>}
        </div>
      </header>
      {error && <div className="corp-admin-error" role="alert">{error}</div>}
      <main aria-busy={busy}>{children}</main>
    </section>
  </div>;
}
