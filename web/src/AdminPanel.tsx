import { Fragment, useEffect, useRef, useState } from 'react';
import { Archive, ChevronDown, ChevronRight, Pencil, Plus, RotateCcw, Shield } from 'lucide-react';
import { AccessGrant, client, Space, Team, UserAccessSnapshot } from './api';
import MCPPersistentAccess from './MCPPersistentAccess';
import { AgentProjects } from './AgentProjects';
import { CatalogKind, CatalogSort, DEFAULT_CATALOG_QUERY, catalogQueryString, parseCatalogQuery } from './catalogQuery';
import { currentUrl, lastUrlRef, replaceUrl } from './nav';
import { GeneralSpaceSettings } from './GeneralSpaceSettings';
import { ArtifactSettings } from './artifacts/ArtifactSettings';
import { AdminWorkspace } from './AdminWorkspace';
import { AdminSpaceArchiveDialog, AdminSpaceEditDialog } from './AdminSpaceDialogs';
import { AdminTeams } from './AdminTeams';
import './AdminPanel.css';

type UserRow = { id: string; name: string; disabled: boolean; admin: boolean };
export type AdminTab = 'catalog' | 'teams' | 'users' | 'projects' | 'tokens' | 'settings';

export const readAdminTab = (): AdminTab => {
  const tab = new URLSearchParams(location.search).get('tab');
  return tab === 'teams' || tab === 'users' || tab === 'projects' || tab === 'tokens' || tab === 'settings' ? tab : 'catalog';
};
export const adminUrl = (tab: AdminTab = 'catalog') => `/?view=admin&tab=${tab}`;
type Role = AccessGrant['role'];

type SpaceEditTarget = { id: string; initialTab: 'general' | 'access' };
type CatalogGroup = 'active' | 'archive';
type UserAccessState = { userId: string; status: 'loading' | 'ready' | 'error'; data?: UserAccessSnapshot; error?: string };

const ROLE_LABELS: Record<Role, string> = { owner: 'Владелец', manager: 'Менеджер', editor: 'Редактор', commenter: 'Комментатор', reader: 'Читатель' };
const roleLabel = (role?: string) => ROLE_LABELS[role as Role] || role || '—';
const canManage = (role?: string) => role === 'manager' || role === 'owner';
const message = (e: unknown) => (e instanceof Error ? e.message : String(e));

export function AdminPanel({ onClose, userId, isAdmin, personalMode = false, activeTab, onTabChange }: { onClose: () => void; userId: string; isAdmin: boolean; personalMode?: boolean; activeTab: AdminTab; onTabChange: (tab: AdminTab) => void }) {
  // Personal mode has no MCP keys or delegations (docs/PERSONAL.md).
  const tab = (!isAdmin && (activeTab === 'teams' || activeTab === 'users' || activeTab === 'settings')) || (personalMode && activeTab === 'tokens') ? 'catalog' : activeTab;
  const setTab = onTabChange;
  const initialCatalog = tab === 'catalog' ? parseCatalogQuery(location.search) : DEFAULT_CATALOG_QUERY;
  const [activeSpaces, setActiveSpaces] = useState<Space[]>([]);
  const [archivedSpaces, setArchivedSpaces] = useState<Space[]>([]);
  const [teams, setTeams] = useState<Team[]>([]);
  const [users, setUsers] = useState<UserRow[]>([]);
  const [spaceEdit, setSpaceEdit] = useState<SpaceEditTarget | null>(
    initialCatalog.cspace ? { id: initialCatalog.cspace, initialTab: 'access' } : null,
  );
  const [spaceArchive, setSpaceArchive] = useState<Space | null>(null);
  const [catalogGroup, setCatalogGroup] = useState<CatalogGroup>('active');
  const [createSpaceOpen, setCreateSpaceOpen] = useState(false);
  const [userFilter, setUserFilter] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [catalogFilter, setCatalogFilter] = useState(initialCatalog.q);
  const [catalogSort, setCatalogSort] = useState<CatalogSort>(initialCatalog.sort);
  const [catalogKind, setCatalogKind] = useState<CatalogKind>(initialCatalog.kind);
  const [userAccess, setUserAccess] = useState<UserAccessState | null>(null);
  const refreshSeq = useRef(0);
  const userAccessSeq = useRef(0);

  const refresh = async () => {
    const seq = ++refreshSeq.current;
    setBusy(true); setError('');
    try {
      const wantTeams = isAdmin && (tab === 'teams' || tab === 'catalog');
      const wantUsers = isAdmin && (tab === 'teams' || tab === 'users');
      const [catalogResult, archiveResult, teamsResult, usersResult] = await Promise.allSettled([
        tab === 'catalog' ? client.catalog() : Promise.resolve(null),
        tab === 'catalog' ? client.spaces(true) : Promise.resolve(null),
        wantTeams ? client.teams() : Promise.resolve(null),
        wantUsers ? client.adminUsers() : Promise.resolve(null),
      ]);
      if (seq !== refreshSeq.current) return;
      const errors: string[] = [];
      const loadedCatalog = catalogResult.status === 'fulfilled' ? catalogResult.value : null;
      if (loadedCatalog) setActiveSpaces(loadedCatalog);
      else if (catalogResult.status === 'rejected') errors.push(`Каталог: ${message(catalogResult.reason)}`);
      if (archiveResult.status === 'fulfilled' && archiveResult.value) setArchivedSpaces(archiveResult.value.filter(space => space.archived_at));
      else if (archiveResult.status === 'rejected') {
        errors.push(`Архив: ${message(archiveResult.reason)}`);
        if (loadedCatalog) {
          const activeIds = new Set(loadedCatalog.map(space => space.id));
          setArchivedSpaces(current => current.filter(space => !activeIds.has(space.id)));
        }
      }
      if (teamsResult.status === 'fulfilled' && teamsResult.value) {
        const loadedTeams = teamsResult.value;
        setTeams(loadedTeams);
      } else if (teamsResult.status === 'rejected') errors.push(`Команды: ${message(teamsResult.reason)}`);
      if (usersResult.status === 'fulfilled' && usersResult.value) setUsers(usersResult.value);
      else if (usersResult.status === 'rejected') errors.push(`Пользователи: ${message(usersResult.reason)}`);
      setError(errors.join(' · '));
    } finally { if (seq === refreshSeq.current) setBusy(false); }
  };
  // Catalog state lives in the URL: re-read it on entering the tab (a fresh visit has none) and on Back / Forward.
  const pendingSpace = useRef(''); // chosen via openResource before the tab URL (which carries no cspace) is applied
  const applyCatalogUrl = () => {
    const next = parseCatalogQuery(location.search);
    const chosen = next.cspace || pendingSpace.current;
    pendingSpace.current = '';
    setCatalogFilter(next.q); setCatalogSort(next.sort); setCatalogKind(next.kind);
    setSpaceArchive(null);
    setSpaceEdit(chosen ? { id: chosen, initialTab: history.state?.aiwikiCatalogEditTab === 'general' ? 'general' : 'access' } : null);
  };
  useEffect(() => {
    if (tab !== 'catalog') return;
    setCatalogGroup('active');
    applyCatalogUrl();
  }, [tab]);
  useEffect(() => {
    const onPop = () => { const params = new URLSearchParams(location.search); if (params.get('view') === 'admin' && readAdminTab() === 'catalog') applyCatalogUrl(); };
    window.addEventListener('popstate', onPop);
    return () => window.removeEventListener('popstate', onPop);
  }, []);
  const catalogUrlFor = (cspace: string, params = new URLSearchParams(location.search)) => {
    const query = catalogQueryString({ q: catalogFilter, sort: catalogSort, kind: catalogKind, cspace }, params);
    return `${location.pathname}?${query}${location.hash}`;
  };
  const urlQ = useRef(catalogFilter);
  useEffect(() => {
    if (tab !== 'catalog') return;
    const typed = urlQ.current !== catalogFilter; urlQ.current = catalogFilter;
    const timer = setTimeout(() => {
      const params = new URLSearchParams(location.search);
      if (params.get('view') !== 'admin' || readAdminTab() !== 'catalog') return;
      const url = catalogUrlFor(spaceEdit?.id || '', params);
      if (url !== currentUrl()) replaceUrl(url);
    }, typed ? 300 : 0);
    return () => clearTimeout(timer);
  }, [tab, catalogFilter, catalogSort, catalogKind, spaceEdit?.id]);

  useEffect(() => {
    if (tab !== 'tokens' && tab !== 'projects') void refresh();
  }, [tab]);

  const editSpace = archivedSpaces.find(space => space.id === spaceEdit?.id)
    ?? activeSpaces.find(space => space.id === spaceEdit?.id);
  const editSpaceArchived = !!editSpace?.archived_at;
  useEffect(() => {
    if (spaceEdit) setCatalogGroup(editSpaceArchived ? 'archive' : 'active');
  }, [spaceEdit, editSpaceArchived]);

  const run = async (action: () => Promise<unknown>, onError: (text: string) => void = setError) => {
    setBusy(true); setError('');
    try { await action(); } catch (e) { onError(message(e)); setBusy(false); return false; }
    try { await refresh(); } finally { setBusy(false); }
    return true;
  };

  const toggleUserAccess = async (user: UserRow) => {
    const seq = ++userAccessSeq.current;
    if (userAccess?.userId === user.id) { setUserAccess(null); return; }
    setUserAccess({ userId: user.id, status: 'loading' });
    try {
      const data = await client.adminUserAccess(user.id);
      if (seq === userAccessSeq.current) setUserAccess({ userId: user.id, status: 'ready', data });
    } catch (e) {
      if (seq === userAccessSeq.current) setUserAccess({ userId: user.id, status: 'error', error: message(e) });
    }
  };
  const openResource = (id: string) => { pendingSpace.current = id; setTab('catalog'); };
  const openSpaceEdit = (id: string, initialTab: SpaceEditTarget['initialTab']) => {
    // Commit any search text still waiting on the debounce so Back restores the visible filters.
    const previousUrl = catalogUrlFor(parseCatalogQuery(location.search).cspace);
    if (previousUrl !== currentUrl()) replaceUrl(previousUrl);
    const url = catalogUrlFor(id);
    if (url !== previousUrl) {
      history.pushState({ ...history.state, aiwikiCatalogEditTab: initialTab }, '', url);
      lastUrlRef.current = url;
    }
    setSpaceEdit({ id, initialTab });
  };
  const closeSpaceEdit = () => {
    if (history.state?.aiwikiCatalogEditTab) history.back();
    else setSpaceEdit(null);
  };
  const archivedSpaceIds = new Set(archivedSpaces.map(space => space.id));
  const visibleActiveSpaces = activeSpaces.filter(space => !archivedSpaceIds.has(space.id));
  const groupSpaces = catalogGroup === 'active' ? visibleActiveSpaces : archivedSpaces;
  const catalogNeedle = catalogFilter.trim().toLocaleLowerCase();
  const catalogSpaces = groupSpaces.filter(space => {
    if (catalogKind === 'personal' && !space.personal) return false;
    if (catalogKind === 'team' && space.personal) return false;
    if (!catalogNeedle) return true;
    return [space.name, ...(space.tags || [])].join(' ').toLocaleLowerCase().includes(catalogNeedle);
  }).sort((a, b) => catalogSort === 'name'
    ? a.name.localeCompare(b.name, 'ru')
    : catalogSort === 'count'
      ? (b.article_count ?? 0) - (a.article_count ?? 0) || a.name.localeCompare(b.name, 'ru')
      : (Date.parse(b.last_updated || '') || 0) - (Date.parse(a.last_updated || '') || 0) || a.name.localeCompare(b.name, 'ru'));
  const userNeedle = userFilter.trim().toLocaleLowerCase();
  const matchingUsers = users.filter(user => [user.name, user.id].some(value => value?.toLocaleLowerCase().includes(userNeedle)));

  const userAccessPanel = (user: UserRow) => {
    if (userAccess?.userId !== user.id) return null;
    const resources = userAccess.data?.resources || [];
    return <div className="corp-admin-user-access" id={`user-access-${user.id}`}>
      <p className="corp-admin-hint">Показаны ресурсы, которыми вы управляете. Список только для просмотра.</p>
      {userAccess.status === 'loading' && <p className="corp-admin-empty">Загрузка…</p>}
      {userAccess.status === 'error' && <p className="corp-admin-inline-error" role="alert">Не удалось загрузить доступ: {userAccess.error}</p>}
      {userAccess.status === 'ready' && !resources.length && <p className="corp-admin-empty">Среди ресурсов, которыми вы управляете, у этого пользователя нет доступа.</p>}
      {resources.map(resource => <button type="button" className="corp-admin-resource" key={`${resource.resource_type}:${resource.resource_id}`} onClick={() => openResource(resource.resource_id)} title={`Открыть: ${resource.name}`}>
        <span className="corp-admin-resource-name">{resource.name || resource.resource_id}</span>
        <span className="corp-admin-badges">
          <span className="corp-admin-badge">Пространство</span>
          {resource.personal && <span className="corp-admin-badge">Личное</span>}
          {resource.archived && <span className="corp-admin-badge is-muted">В архиве</span>}
          <span className="corp-admin-badge">{roleLabel(resource.role)}</span>
        </span>
        <small className="corp-admin-id" title={resource.resource_id}>{resource.resource_id}</small>
      </button>)}
    </div>;
  };

  return <>
    <AdminWorkspace active={tab} onNavigate={setTab} onClose={onClose} isAdmin={isAdmin} personalMode={personalMode}
      busy={busy} error={error} onRefresh={tab === 'catalog' || tab === 'teams' || tab === 'users' ? () => void refresh() : undefined}>
        {tab === 'tokens' && <MCPPersistentAccess/>}
        {tab === 'projects' && <AgentProjects userId={userId} isAdmin={isAdmin}/>}
        {tab === 'settings' && isAdmin && <div className="admin-settings-grid">
          <div className="admin-work-panel"><GeneralSpaceSettings/></div>
          <div className="admin-work-panel"><ArtifactSettings/></div>
        </div>}
        {tab === 'catalog' && <>
          <section className="admin-work-panel" aria-label="Каталог пространств">
            <div className="admin-work-toolbar">
              <div className="admin-space-catalog-tabs" role="group" aria-label="Состояние пространств">
                <button className="button" type="button" aria-pressed={catalogGroup === 'active'} onClick={() => setCatalogGroup('active')}>Активные</button>
                <button className="button" type="button" aria-pressed={catalogGroup === 'archive'} onClick={() => setCatalogGroup('archive')}>Архив</button>
              </div>
              <button className="button primary" type="button" onClick={() => setCreateSpaceOpen(true)} disabled={busy}><Plus size={15}/>Создать пространство</button>
              <input type="search" value={catalogFilter} onChange={e => setCatalogFilter(e.target.value)} placeholder="Поиск по названиям и тегам" aria-label="Фильтр каталога"/>
              <select value={catalogKind} onChange={e => setCatalogKind(e.target.value as CatalogKind)} aria-label="Тип пространств">
                <option value="all">Все типы</option><option value="personal">Личные</option><option value="team">Командные</option>
              </select>
              <select value={catalogSort} onChange={e => setCatalogSort(e.target.value as CatalogSort)} aria-label="Сортировка каталога">
                <option value="recent">По обновлению</option><option value="name">По названию</option><option value="count">По числу документов</option>
              </select>
            </div>
            {catalogSpaces.length > 0 && <div className="admin-work-table-wrap">
              <table className="admin-work-table" aria-label="Пространства">
                <thead><tr>
                  <th scope="col">Название</th><th scope="col">Тип</th><th scope="col">Документы</th><th scope="col">Изменено</th><th scope="col">Действия</th>
                </tr></thead>
                <tbody>{catalogSpaces.map(space => {
                  const managesSpace = canManage(space.effective_role) && !space.limited;
                  const canOpenSpace = managesSpace || (isAdmin && space.limited);
                  const editLabel = space.limited ? `Доступ к пространству «${space.name}»` : `Редактировать пространство «${space.name}»`;
                  const archiveLabel = space.archived_at ? `Восстановить пространство «${space.name}»` : `Архивировать пространство «${space.name}»`;
                  const EditIcon = space.limited ? Shield : Pencil;
                  const ArchiveIcon = space.archived_at ? RotateCcw : Archive;
                  return <tr key={space.id}>
                    <th scope="row">
                      <strong>{space.name}</strong>
                      {space.limited && <small className="corp-admin-id">{space.owner_name ? `Владелец: ${space.owner_name} · ` : ''}Нет доступа к содержимому</small>}
                      {!space.limited && !!space.tags?.length && <span className="corp-admin-tags">{space.tags.map(tag => <span key={tag}>{tag}</span>)}</span>}
                    </th>
                    <td><span className="corp-admin-badge">{space.personal ? 'Личное' : 'Командное'}</span></td>
                    <td>{space.limited ? '—' : space.article_count ?? '—'}</td>
                    <td>{!space.limited && space.last_updated ? <time dateTime={space.last_updated}>{new Date(space.last_updated).toLocaleDateString()}</time> : '—'}</td>
                    <td><div className="admin-work-table-actions admin-space-row-actions">
                      {canOpenSpace && <button className="icon-button" type="button" title={editLabel} aria-label={editLabel} aria-haspopup="dialog"
                        onClick={() => openSpaceEdit(space.id, space.limited ? 'access' : 'general')}>
                        <EditIcon size={16} aria-hidden="true"/>
                      </button>}
                      {managesSpace && <button className="icon-button" type="button" title={archiveLabel} aria-label={archiveLabel} disabled={busy}
                        aria-haspopup={space.archived_at ? undefined : 'dialog'} onClick={() => {
                          if (space.archived_at) void run(() => client.updateSpace(space.id, { archived: false }));
                          else setSpaceArchive(space);
                        }}>
                        <ArchiveIcon size={16} aria-hidden="true"/>
                      </button>}
                    </div></td>
                  </tr>;
                })}</tbody>
              </table>
            </div>}
            {!catalogSpaces.length && (groupSpaces.length > 0 && (catalogNeedle || catalogKind !== 'all')
              ? <div className="corp-admin-empty corp-admin-empty-filter" role="status"><p>Ничего не найдено</p><button type="button" className="button ghost" onClick={() => { setCatalogFilter(''); setCatalogKind('all'); }}>Сбросить фильтры</button></div>
              : <p className="corp-admin-empty" role="status">{busy ? 'Загрузка…' : catalogGroup === 'archive' ? 'В архиве пока нет пространств.' : 'Активных пространств нет.'}</p>)}
          </section>
          {spaceEdit && editSpace && (canManage(editSpace.effective_role) || (isAdmin && editSpace.limited)) &&
            <AdminSpaceEditDialog key={editSpace.id} space={editSpace} teams={teams} isAdmin={isAdmin} personalMode={personalMode} initialTab={spaceEdit.initialTab}
              onChanged={() => void refresh()} onClose={closeSpaceEdit}/>}
          {spaceArchive && <AdminSpaceArchiveDialog space={spaceArchive}
            onChanged={() => void refresh()} onClose={() => setSpaceArchive(null)}/>}
          {createSpaceOpen && <AdminSpaceEditDialog teams={teams} isAdmin={isAdmin} personalMode={personalMode} initialTab="general"
            onChanged={() => { setCatalogGroup('active'); void refresh(); }} onClose={() => setCreateSpaceOpen(false)}/>}
        </>}
        {tab === 'teams' && <AdminTeams teams={teams} users={users} loading={busy} onChanged={() => void refresh()}/>}
        {tab === 'users' && <section className="admin-work-panel" aria-label="Список пользователей">
          <div className="admin-work-toolbar">
            <input type="search" aria-label="Поиск по пользователям" placeholder="Поиск по имени и ID" value={userFilter} onChange={e => setUserFilter(e.target.value)}/>
          </div>
          {matchingUsers.length > 0 && <div className="admin-work-table-wrap">
            <table className="admin-work-table" aria-label="Пользователи">
              <thead><tr><th scope="col">Пользователь</th><th scope="col">Роль</th><th scope="col">Статус</th><th scope="col">Действия</th></tr></thead>
              <tbody>{matchingUsers.map(user => {
                const expanded = userAccess?.userId === user.id;
                return <Fragment key={user.id}>
                  <tr data-selected={expanded || undefined}>
                    <th scope="row"><strong>{user.name || user.id}</strong><small className="corp-admin-id" title={user.id}>{user.id}</small></th>
                    <td>{user.admin ? 'Администратор' : 'Пользователь'}</td>
                    <td><span className={`corp-admin-badge${user.disabled ? ' is-muted' : ''}`}>{user.disabled ? 'Отключён' : 'Активен'}</span></td>
                    <td><div className="admin-work-table-actions">
                      <button className="button ghost" type="button" onClick={() => void toggleUserAccess(user)} aria-expanded={expanded}
                        aria-label={`Пространства пользователя «${user.name || user.id}»`} aria-controls={expanded ? `user-access-${user.id}` : undefined}>
                        {expanded ? <ChevronDown size={14}/> : <ChevronRight size={14}/>}Пространства
                      </button>
                      <button className="button ghost" type="button" onClick={() => void run(async () => {
                        await client.setUserDisabled(user.id, !user.disabled);
                        userAccessSeq.current++;
                        setUserAccess(null);
                      })} disabled={busy || user.id === userId} title={user.id === userId ? 'Нельзя отключить себя' : undefined}>
                        <Shield size={14}/>{user.disabled ? 'Включить' : 'Отключить'}
                      </button>
                    </div></td>
                  </tr>
                  {expanded && <tr><td colSpan={4}>{userAccessPanel(user)}</td></tr>}
                </Fragment>;
              })}</tbody>
            </table>
          </div>}
          {!matchingUsers.length && <p className="corp-admin-empty" role="status">{busy ? 'Загрузка…' : users.length ? 'Пользователи по этому запросу не найдены.' : 'Пользователи не найдены или список недоступен.'}</p>}
        </section>}
    </AdminWorkspace>
  </>;
}
