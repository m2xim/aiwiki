import { FormEvent, useEffect, useRef, useState } from 'react';
import { Archive, ArrowLeft, ChevronDown, ChevronRight, Pencil, Plus, RefreshCw, RotateCcw, Shield } from 'lucide-react';
import { AccessGrant, CatalogSpace, client, Project, Team, TeamMember, UserAccessSnapshot } from './api';
import MCPDelegations from './MCPDelegations';
import MCPPersistentAccess from './MCPPersistentAccess';
import './AdminPanel.css';

type UserRow = { id: string; name: string; disabled: boolean; admin: boolean };
export type AdminTab = 'catalog' | 'teams' | 'users' | 'projects' | 'tokens';
type Tab = AdminTab;

export const readAdminTab = (): AdminTab => {
  const tab = new URLSearchParams(location.search).get('tab');
  return tab === 'teams' || tab === 'users' || tab === 'projects' || tab === 'tokens' ? tab : 'catalog';
};
export const adminUrl = (tab: AdminTab = 'catalog') => `/?view=admin&tab=${tab}`;
type Role = AccessGrant['role'];
type ResourceType = 'space' | 'project';
type EditDraft = { id: string; name: string; description: string };
type UserAccessState = { userId: string; status: 'loading' | 'ready' | 'error'; data?: UserAccessSnapshot; error?: string };

const ROLE_LABELS: Record<Role, string> = { owner: 'Владелец', manager: 'Менеджер', editor: 'Редактор', commenter: 'Комментатор', reader: 'Читатель' };
const roleLabel = (role?: string) => ROLE_LABELS[role as Role] || role || '—';
const canManage = (role?: string) => role === 'manager' || role === 'owner';
const message = (e: unknown) => (e instanceof Error ? e.message : String(e));

export function AdminPanel({ onClose, userId, isAdmin, activeTab, onTabChange }: { onClose: () => void; userId: string; isAdmin: boolean; activeTab: AdminTab; onTabChange: (tab: AdminTab) => void }) {
  const tab = !isAdmin && (activeTab === 'teams' || activeTab === 'users') ? 'catalog' : activeTab;
  const setTab = onTabChange;
  const [spaces, setSpaces] = useState<CatalogSpace[]>([]);
  const [projects, setProjects] = useState<Project[]>([]);
  const [teams, setTeams] = useState<Team[]>([]);
  const [users, setUsers] = useState<UserRow[]>([]);
  const [directory, setDirectory] = useState<TeamMember[]>([]);
  const [teamMembers, setTeamMembers] = useState<TeamMember[]>([]);
  const [grants, setGrants] = useState<AccessGrant[]>([]);
  const [accessLoading, setAccessLoading] = useState(false);
  const [accessError, setAccessError] = useState('');
  const [selectedTeam, setSelectedTeam] = useState('');
  const [selectedSpace, setSelectedSpace] = useState('');
  const [selectedProject, setSelectedProject] = useState('');
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [personal, setPersonal] = useState(true);
  const [projectSpace, setProjectSpace] = useState('');
  const [projectParent, setProjectParent] = useState('');
  const [projectSpaceFilter, setProjectSpaceFilter] = useState('');
  const [targetId, setTargetId] = useState('');
  const [subjectType, setSubjectType] = useState<'user' | 'team'>('user');
  const [role, setRole] = useState<Role>('reader');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [catalogFilter, setCatalogFilter] = useState('');
  const [catalogSort, setCatalogSort] = useState<'recent' | 'name' | 'count'>('recent');
  const [editProject, setEditProject] = useState<EditDraft | null>(null);
  const [editTeam, setEditTeam] = useState<EditDraft | null>(null);
  const [editError, setEditError] = useState('');
  const [memberPick, setMemberPick] = useState('');
  const [membersLoading, setMembersLoading] = useState(false);
  const [membersError, setMembersError] = useState('');
  const [userAccess, setUserAccess] = useState<UserAccessState | null>(null);
  const refreshSeq = useRef(0);
  const accessSeq = useRef(0);
  const membersSeq = useRef(0);
  const userAccessSeq = useRef(0);

  const refresh = async () => {
    const seq = ++refreshSeq.current;
    setBusy(true); setError('');
    try {
      const wantTeams = isAdmin && (tab === 'teams' || tab === 'catalog' || tab === 'projects');
      const wantUsers = isAdmin && (tab === 'teams' || tab === 'users');
      const [catalogResult, teamsResult, usersResult, projectsResult] = await Promise.allSettled([
        client.catalog(), wantTeams ? client.teams() : Promise.resolve(null), wantUsers ? client.adminUsers() : Promise.resolve(null), client.projects(),
      ]);
      if (seq !== refreshSeq.current) return;
      const errors: string[] = [];
      const catalogProjects = new Map<string, Project>();
      if (catalogResult.status === 'fulfilled') {
        const visibleSpaces = catalogResult.value || [];
        visibleSpaces.forEach(space => (space.projects || []).forEach(project => catalogProjects.set(project.id, project)));
        const spaceIds = visibleSpaces.map(space => space.id);
        setSpaces(visibleSpaces);
        setProjects([...catalogProjects.values()]);
        setSelectedSpace(current => spaceIds.includes(current) ? current : spaceIds[0] || '');
        setProjectSpace(current => spaceIds.includes(current) ? current : spaceIds[0] || '');
        setProjectSpaceFilter(current => !current || spaceIds.includes(current) ? current : '');
      } else errors.push(`Каталог: ${message(catalogResult.reason)}`);
      if (teamsResult.status === 'fulfilled' && teamsResult.value) {
        const loadedTeams = teamsResult.value;
        setTeams(loadedTeams);
        setSelectedTeam(current => loadedTeams.some(team => team.id === current) ? current : loadedTeams[0]?.id || '');
      } else if (teamsResult.status === 'rejected') errors.push(`Команды: ${message(teamsResult.reason)}`);
      if (usersResult.status === 'fulfilled' && usersResult.value) setUsers(usersResult.value);
      else if (usersResult.status === 'rejected') errors.push(`Пользователи: ${message(usersResult.reason)}`);
      if (projectsResult.status === 'fulfilled') {
        const merged = projectsResult.value.map(project => {
          const known = catalogProjects.get(project.id);
          return { ...known, ...project, effective_role: project.effective_role || known?.effective_role, article_count: project.article_count ?? known?.article_count, last_updated: project.last_updated || known?.last_updated, tags: project.tags || known?.tags };
        });
        setProjects(merged);
        setSelectedProject(current => !current || merged.some(project => project.id === current) ? current : '');
      } else if (catalogResult.status !== 'fulfilled') errors.push(`Проекты: ${message(projectsResult.reason)}`);
      setError(errors.join(' · '));
    } finally { if (seq === refreshSeq.current) setBusy(false); }
  };
  useEffect(() => {
    setEditProject(null); setEditTeam(null); setEditError(''); setName(''); setDescription(''); setTargetId(''); setMemberPick('');
    if (tab !== 'tokens') void refresh();
  }, [tab]);

  // Team members of the selected team.
  const loadMembers = async (teamId: string) => {
    const seq = ++membersSeq.current;
    setMembersLoading(true); setMembersError('');
    try {
      const members = await client.teamMembers(teamId);
      if (seq === membersSeq.current) setTeamMembers(members || []);
    } catch (e) {
      if (seq === membersSeq.current) { setTeamMembers([]); setMembersError(`Участники команды: ${message(e)}`); }
    } finally { if (seq === membersSeq.current) setMembersLoading(false); }
  };
  useEffect(() => {
    setEditTeam(null); setEditError(''); setMemberPick(''); setTeamMembers([]); setMembersError('');
    if (!selectedTeam || tab !== 'teams') { membersSeq.current++; setMembersLoading(false); return; }
    void loadMembers(selectedTeam);
    return () => { membersSeq.current++; };
  }, [selectedTeam, tab]);

  // Access grants: only for a selected, non-limited resource where the actor is manager/owner.
  const accessType: ResourceType | '' = tab === 'catalog' ? 'space' : tab === 'projects' ? 'project' : '';
  const accessId = accessType === 'space' ? selectedSpace : accessType === 'project' ? selectedProject : '';
  const selectedSpaceEntry = spaces.find(space => space.id === selectedSpace);
  const selectedProjectEntry = projects.find(project => project.id === selectedProject);
  const accessResource = accessType === 'space' ? selectedSpaceEntry : accessType === 'project' ? selectedProjectEntry : undefined;
  const accessAllowed = !!accessResource && canManage(accessResource.effective_role) && !(accessType === 'space' && selectedSpaceEntry?.limited);
  const accessKey = accessAllowed ? `${accessType}:${accessId}` : '';
  const loadAccess = async (type: ResourceType, id: string) => {
    const seq = ++accessSeq.current;
    setAccessLoading(true); setAccessError('');
    const [grantResult, userResult] = await Promise.allSettled([client.access(type, id), client.users(type, id)]);
    if (seq !== accessSeq.current) return;
    const errors: string[] = [];
    if (grantResult.status === 'fulfilled') setGrants(grantResult.value || []); else { setGrants([]); errors.push(`Список доступа: ${message(grantResult.reason)}`); }
    if (userResult.status === 'fulfilled') setDirectory(userResult.value || []); else { setDirectory([]); errors.push(`Список пользователей для выдачи доступа: ${message(userResult.reason)}`); }
    setAccessError(errors.join(' · ')); setAccessLoading(false);
  };
  useEffect(() => {
    setGrants([]); setDirectory([]); setAccessError(''); setTargetId('');
    if (!accessKey) { accessSeq.current++; setAccessLoading(false); return; }
    void loadAccess(accessType as ResourceType, accessId);
    return () => { accessSeq.current++; };
  }, [accessKey]);

  const run = async (action: () => Promise<unknown>, onError: (text: string) => void = setError) => {
    setBusy(true); setError('');
    try { await action(); } catch (e) { onError(message(e)); setBusy(false); return false; }
    try { await refresh(); } finally { setBusy(false); }
    return true;
  };

  const create = async () => {
    if (!name.trim() || busy) return;
    await run(async () => {
      if (tab === 'teams') {
        const created = await client.createTeam(name.trim(), description.trim());
        if (created?.id) setSelectedTeam(created.id);
      } else if (tab === 'catalog') await client.createSpaceFor({ name: name.trim(), description: description.trim(), personal });
      else if (projectSpace) await client.createProject({ name: name.trim(), description: description.trim(), space_id: projectSpace, parent_id: projectParent, inherit_acl: true });
      else throw new Error('Сначала выберите пространство для проекта.');
      setName(''); setDescription('');
    });
  };
  const submit = (action: () => void) => (e: FormEvent) => { e.preventDefault(); action(); };

  const addGrant = async () => {
    if (!targetId.trim() || !accessKey || busy) return;
    const type = accessType as ResourceType;
    await run(async () => {
      await client.setAccess(type, accessId, { subject_type: subjectType, subject_id: targetId.trim(), role });
      setTargetId('');
      await loadAccess(type, accessId);
    }, setAccessError);
  };
  const removeGrant = (grant: AccessGrant) => run(async () => {
    await client.removeAccess(accessType as ResourceType, accessId, grant);
    await loadAccess(accessType as ResourceType, accessId);
  }, setAccessError);

  const subjectName = (grant: AccessGrant) => grant.subject_type === 'team'
    ? teams.find(team => team.id === grant.subject_id)?.name
    : directory.find(user => user.id === grant.subject_id)?.name;
  const grantTeams = teams.filter(team => !team.archived_at);
  const grantEditor = (resourceArchived: boolean) => <section className="corp-admin-access" aria-label="Доступ">
    <h3>Доступ{accessResource ? `: ${accessResource.name}` : ''}</h3>
    {accessError && <p className="corp-admin-inline-error" role="alert">{accessError}</p>}
    {resourceArchived
      ? <p className="corp-admin-empty">Проект в архиве: изменение доступа недоступно. Сначала восстановите проект.</p>
      : <form className="corp-admin-grant" onSubmit={submit(() => void addGrant())}>
        <select aria-label="Тип получателя" value={subjectType} onChange={e => { setSubjectType(e.target.value as 'user' | 'team'); setTargetId(''); }}><option value="user">Пользователь</option><option value="team">Команда</option></select>
        {subjectType === 'user'
          ? <select aria-label="Пользователь" value={targetId} onChange={e => setTargetId(e.target.value)}><option value="">Пользователь…</option>{directory.map(user => <option key={user.id} value={user.id}>{user.name || user.id} · {user.id}</option>)}</select>
          : grantTeams.length > 0
            ? <select aria-label="Команда" value={targetId} onChange={e => setTargetId(e.target.value)}><option value="">Команда…</option>{grantTeams.map(team => <option key={team.id} value={team.id}>{team.name}</option>)}</select>
            : <input aria-label="ID команды" value={targetId} onChange={e => setTargetId(e.target.value)} placeholder="ID команды"/>}
        <select aria-label="Роль" value={role} onChange={e => setRole(e.target.value as Role)}>{(Object.keys(ROLE_LABELS) as Role[]).map(key => <option key={key} value={key}>{ROLE_LABELS[key]}</option>)}</select>
        <button type="submit" disabled={busy || accessLoading || !targetId.trim()}>Выдать роль</button>
      </form>}
    {accessLoading && <p className="corp-admin-empty">Загрузка списка доступа…</p>}
    {!accessLoading && !accessError && !grants.length && <p className="corp-admin-empty">Явных выдач доступа пока нет.</p>}
    {grants.map(grant => <article className="corp-admin-row" key={`${grant.subject_type}:${grant.subject_id}`}>
      <div className="corp-admin-row-main"><strong title={grant.subject_id}>{grant.subject_type === 'team' ? 'Команда' : 'Пользователь'} · {subjectName(grant) || grant.subject_id}</strong><small className="corp-admin-id" title={grant.subject_id}>{grant.subject_id}</small></div>
      <span className="corp-admin-badge">{roleLabel(grant.role)}</span>
      <div className="corp-admin-row-actions"><button type="button" onClick={() => void removeGrant(grant)} disabled={busy}>Убрать доступ</button></div>
    </article>)}
  </section>;

  const saveProject = () => {
    if (!editProject || !editProject.name.trim() || busy) return;
    setEditError('');
    void run(async () => { await client.updateProject(editProject.id, { name: editProject.name.trim(), description: editProject.description.trim() }); setEditProject(null); }, setEditError);
  };
  const saveTeam = () => {
    if (!editTeam || !editTeam.name.trim() || busy) return;
    setEditError('');
    void run(async () => { await client.updateTeam(editTeam.id, { name: editTeam.name.trim(), description: editTeam.description.trim() }); setEditTeam(null); }, setEditError);
  };
  const addMember = () => {
    if (!selectedTeam || !memberPick || busy) return;
    const teamId = selectedTeam;
    void run(async () => { await client.setTeamMember(teamId, memberPick); setMemberPick(''); await loadMembers(teamId); }, setMembersError);
  };
  const removeMember = (member: TeamMember) => {
    const teamId = selectedTeam;
    void run(async () => { await client.removeTeamMember(teamId, member.id); await loadMembers(teamId); }, setMembersError);
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
  const openResource = (type: ResourceType, id: string) => {
    if (type === 'space') { setSelectedSpace(id); setTab('catalog'); }
    else { setSelectedProject(id); setProjectSpaceFilter(''); setTab('projects'); }
  };

  const catalogSpaces = spaces.filter(space => {
    const needle = catalogFilter.trim().toLocaleLowerCase();
    if (!needle) return true;
    return [space.name, ...(space.projects || []).flatMap(project => [project.name, ...(project.tags || [])]), ...(space.tags || [])].join(' ').toLocaleLowerCase().includes(needle);
  }).sort((a, b) => catalogSort === 'name'
    ? a.name.localeCompare(b.name, 'ru')
    : catalogSort === 'count'
      ? (b.article_count ?? 0) - (a.article_count ?? 0) || a.name.localeCompare(b.name, 'ru')
      : (Date.parse(b.last_updated || '') || 0) - (Date.parse(a.last_updated || '') || 0) || a.name.localeCompare(b.name, 'ru'));
  const visibleProjects = projects.filter(project => !projectSpaceFilter || project.space_id === projectSpaceFilter);
  const spaceName = (id: string) => spaces.find(space => space.id === id)?.name;
  const team = teams.find(t => t.id === selectedTeam);
  const memberIds = new Set(teamMembers.map(member => member.id));
  const candidates = users.filter(user => !user.disabled && !memberIds.has(user.id));

  const projectRow = (project: Project) => {
    const archived = !!project.archived_at;
    const manager = canManage(project.effective_role);
    const owner = project.effective_role === 'owner';
    const editing = editProject?.id === project.id;
    return <div className={`corp-admin-project-row${archived ? ' is-archived' : ''}${selectedProject === project.id ? ' is-selected' : ''}`} key={project.id}>
      {editing && editProject
        ? <form className="corp-admin-edit" onSubmit={submit(saveProject)}>
          <label>Название проекта<input autoFocus value={editProject.name} onChange={e => setEditProject({ ...editProject, name: e.target.value })} disabled={busy}/></label>
          <label>Описание<input value={editProject.description} onChange={e => setEditProject({ ...editProject, description: e.target.value })} disabled={busy}/></label>
          {editError && <p className="corp-admin-inline-error" role="alert">{editError}</p>}
          <div className="corp-admin-row-actions"><button type="submit" disabled={busy || !editProject.name.trim()}>Сохранить</button><button type="button" onClick={() => { setEditProject(null); setEditError(''); }} disabled={busy}>Отмена</button></div>
        </form>
        : <>
          <button type="button" className="corp-admin-project" onClick={() => setSelectedProject(project.id)} aria-pressed={selectedProject === project.id}>
            <strong title={project.name}>{project.name}</strong>
            <span className="corp-admin-badges">
              <span className="corp-admin-badge">{archived ? 'В архиве' : 'Активен'}</span>
              <span className="corp-admin-badge">{project.inherit_acl ? 'Наследует доступ' : 'Свой список доступа'}</span>
              {project.effective_role && <span className="corp-admin-badge">{roleLabel(project.effective_role)}</span>}
              {!manager && <span className="corp-admin-badge is-muted">Только просмотр</span>}
            </span>
            {project.description && <small className="corp-admin-desc">{project.description}</small>}
            {spaceName(project.space_id) && <small className="corp-admin-id" title={project.space_id}>Пространство: {spaceName(project.space_id)}</small>}
          </button>
          <div className="corp-admin-row-actions">
            {manager && !archived && <button type="button" disabled={busy} onClick={() => { setEditError(''); setEditProject({ id: project.id, name: project.name, description: project.description || '' }); }}><Pencil size={14}/>Изменить</button>}
            {manager && <button type="button" disabled={busy} onClick={() => void run(() => client.updateProject(project.id, { archived: !archived }))}>{archived ? <RotateCcw size={14}/> : <Archive size={14}/>}{archived ? 'Восстановить' : 'Архивировать'}</button>}
            {owner && !archived && <label className="corp-admin-check"><input type="checkbox" checked={project.inherit_acl} disabled={busy} onChange={e => void run(() => client.updateProject(project.id, { inherit_acl: e.target.checked }))}/>Наследовать доступ</label>}
          </div>
        </>}
    </div>;
  };

  const userAccessPanel = (user: UserRow) => {
    if (userAccess?.userId !== user.id) return null;
    const resources = userAccess.data?.resources || [];
    return <div className="corp-admin-user-access" id={`user-access-${user.id}`}>
      <p className="corp-admin-hint">Показаны ресурсы, которыми вы управляете. Список только для просмотра.</p>
      {userAccess.status === 'loading' && <p className="corp-admin-empty">Загрузка…</p>}
      {userAccess.status === 'error' && <p className="corp-admin-inline-error" role="alert">Не удалось загрузить доступ: {userAccess.error}</p>}
      {userAccess.status === 'ready' && !resources.length && <p className="corp-admin-empty">Среди ресурсов, которыми вы управляете, у этого пользователя нет доступа.</p>}
      {resources.map(resource => <button type="button" className="corp-admin-resource" key={`${resource.resource_type}:${resource.resource_id}`} onClick={() => openResource(resource.resource_type, resource.resource_id)} title={`Открыть: ${resource.name}`}>
        <span className="corp-admin-resource-name">{resource.name || resource.resource_id}</span>
        <span className="corp-admin-badges">
          <span className="corp-admin-badge">{resource.resource_type === 'space' ? 'Пространство' : 'Проект'}</span>
          {resource.personal && <span className="corp-admin-badge">Личное</span>}
          {resource.archived && <span className="corp-admin-badge is-muted">В архиве</span>}
          <span className="corp-admin-badge">{roleLabel(resource.role)}</span>
        </span>
        <small className="corp-admin-id" title={resource.resource_id}>{resource.resource_id}</small>
      </button>)}
    </div>;
  };

  return <div className="corp-admin-page">
    <section className="corp-admin" aria-labelledby="corp-admin-title">
      <header><div><span>AIWIKI · УПРАВЛЕНИЕ</span><h1 id="corp-admin-title">Команды и доступ</h1></div><button type="button" onClick={onClose}><ArrowLeft size={18}/>К базе знаний</button></header>
      <nav>{([['catalog', 'Пространства'], ['teams', 'Команды'], ['projects', 'Проекты'], ['users', 'Пользователи'], ['tokens', 'MCP-доступ']] as [Tab, string][]).filter(([key]) => isAdmin || key === 'catalog' || key === 'projects' || key === 'tokens').map(([key, label]) => <button type="button" key={key} className={tab === key ? 'active' : ''} onClick={() => setTab(key)}>{label}</button>)}{tab !== 'tokens' && <button type="button" className="corp-admin-refresh" onClick={() => void refresh()} disabled={busy} aria-label="Обновить"><RefreshCw size={15}/></button>}</nav>
      {error && <div className="corp-admin-error" role="alert">{error}</div>}
      <main aria-busy={busy}>
        {tab === 'tokens' && <><MCPPersistentAccess/><MCPDelegations/></>}
        {tab === 'catalog' && <>
          <form className="corp-admin-create" onSubmit={submit(() => void create())}><input aria-label="Название пространства" value={name} onChange={e => setName(e.target.value)} placeholder="Название пространства"/><input aria-label="Описание пространства" value={description} onChange={e => setDescription(e.target.value)} placeholder="Описание (необязательно)"/><select aria-label="Тип пространства" value={personal ? 'personal' : 'team'} onChange={e => setPersonal(e.target.value === 'personal')}><option value="personal">Личное</option>{isAdmin && <option value="team">Командное</option>}</select><button type="submit" disabled={busy || !name.trim()}><Plus size={15}/>Создать</button></form>
          <div className="corp-admin-catalog-tools"><input value={catalogFilter} onChange={e => setCatalogFilter(e.target.value)} placeholder="Поиск по пространствам, проектам и тегам" aria-label="Фильтр каталога"/><select value={catalogSort} onChange={e => setCatalogSort(e.target.value as typeof catalogSort)} aria-label="Сортировка каталога"><option value="recent">Сначала недавно изменённые</option><option value="name">По названию</option><option value="count">По числу документов</option></select></div>
          {catalogSpaces.map(space => <article className="corp-admin-catalog-entry" key={space.id}><button type="button" className="corp-admin-project" onClick={() => setSelectedSpace(space.id)} aria-pressed={selectedSpace === space.id}><strong title={space.name}>{space.name}</strong><small>{space.limited ? 'Ограниченный доступ' : space.personal ? 'Личное' : 'Командное'}{space.archived_at ? ' · В архиве' : ''} · {space.article_count ?? 0} документов · {space.projects?.length ?? 0} проектов{space.last_updated ? ` · Изменено ${new Date(space.last_updated).toLocaleDateString()}` : ''}</small><span className="corp-admin-tags">{(space.tags || []).map(tag => <span key={tag}>{tag}</span>)}</span></button>{(space.projects || []).length > 0 && <div className="corp-admin-child-projects"><small>Проекты</small>{(space.projects || []).map(project => <button type="button" key={project.id} onClick={() => { setSelectedProject(project.id); setProjectSpaceFilter(''); setTab('projects'); }}><span>{project.name}</span><small>{project.archived_at ? 'В архиве · ' : ''}{project.article_count ?? 0} документов{project.last_updated ? ` · ${new Date(project.last_updated).toLocaleDateString()}` : ''}</small><span className="corp-admin-tags">{(project.tags || []).map(tag => <span key={tag}>{tag}</span>)}</span></button>)}</div>}</article>)}
          {!catalogSpaces.length && <p className="corp-admin-empty">Подходящих пространств не найдено.</p>}
          {selectedSpaceEntry && !selectedSpaceEntry.limited && !canManage(selectedSpaceEntry.effective_role) && <p className="corp-admin-empty">Управление доступом к «{selectedSpaceEntry.name}» доступно менеджерам и владельцам.</p>}
          {accessAllowed && accessType === 'space' && grantEditor(!!selectedSpaceEntry?.archived_at)}
        </>}
        {tab === 'teams' && <>
          <form className="corp-admin-create" onSubmit={submit(() => void create())}><input aria-label="Название команды" value={name} onChange={e => setName(e.target.value)} placeholder="Название команды"/><input aria-label="Описание команды" value={description} onChange={e => setDescription(e.target.value)} placeholder="Описание (необязательно)"/><button type="submit" disabled={busy || !name.trim()}><Plus size={15}/>Создать команду</button></form>
          {!teams.length && !busy && <p className="corp-admin-empty">Команд пока нет. Создайте первую выше.</p>}
          <div className="corp-admin-team-list" role="list">{teams.map(t => <div role="listitem" key={t.id}><button type="button" className="corp-admin-project" aria-pressed={selectedTeam === t.id} onClick={() => setSelectedTeam(t.id)}><strong title={t.name}>{t.name}</strong><span className="corp-admin-badges">{t.archived_at && <span className="corp-admin-badge is-muted">В архиве</span>}</span>{t.description && <small className="corp-admin-desc">{t.description}</small>}</button></div>)}</div>
          {team && <section className="corp-admin-team-detail" aria-label="Выбранная команда">
            {editTeam?.id === team.id
              ? <form className="corp-admin-edit" onSubmit={submit(saveTeam)}>
                <label>Название команды<input autoFocus value={editTeam.name} onChange={e => setEditTeam({ ...editTeam, name: e.target.value })} disabled={busy}/></label>
                <label>Описание<input value={editTeam.description} onChange={e => setEditTeam({ ...editTeam, description: e.target.value })} disabled={busy}/></label>
                {editError && <p className="corp-admin-inline-error" role="alert">{editError}</p>}
                <div className="corp-admin-row-actions"><button type="submit" disabled={busy || !editTeam.name.trim()}>Сохранить</button><button type="button" onClick={() => { setEditTeam(null); setEditError(''); }} disabled={busy}>Отмена</button></div>
              </form>
              : <>
                <div className="corp-admin-team-head">
                  <div className="corp-admin-row-main"><h3 title={team.name}>{team.name}{team.archived_at ? ' · в архиве' : ''}</h3>{team.description && <small className="corp-admin-desc">{team.description}</small>}</div>
                  <div className="corp-admin-row-actions">
                    <button type="button" disabled={busy} onClick={() => { setEditError(''); setEditTeam({ id: team.id, name: team.name, description: team.description || '' }); }}><Pencil size={14}/>Изменить</button>
                    <button type="button" disabled={busy} onClick={() => void run(() => client.updateTeam(team.id, { archived: !team.archived_at }))}>{team.archived_at ? <RotateCcw size={14}/> : <Archive size={14}/>}{team.archived_at ? 'Восстановить' : 'Архивировать'}</button>
                  </div>
                </div>
                <p className="corp-admin-hint">{team.archived_at
                  ? 'Команда в архиве: доступ, выданный через неё, может не действовать, а состав участников менять нельзя. Участники сохранены — после восстановления команда вернётся без потерь.'
                  : 'Архивация отключает команду: доступ, выданный через неё, может перестать действовать. Участники сохраняются, команду можно восстановить в любой момент.'}</p>
              </>}
            {editError && editTeam?.id !== team.id && <p className="corp-admin-inline-error" role="alert">{editError}</p>}
            <h3>Участники</h3>
            {membersError && <p className="corp-admin-inline-error" role="alert">{membersError}</p>}
            <form className="corp-admin-grant" onSubmit={submit(addMember)}>
              <select aria-label="Пользователь для добавления" value={memberPick} onChange={e => setMemberPick(e.target.value)} disabled={busy || !!team.archived_at}><option value="">{candidates.length ? 'Выберите пользователя…' : 'Нет доступных пользователей'}</option>{candidates.map(user => <option key={user.id} value={user.id}>{user.name || user.id} · {user.id}</option>)}</select>
              <button type="submit" disabled={busy || !memberPick || !!team.archived_at}>Добавить</button>
            </form>
            {membersLoading && <p className="corp-admin-empty">Загрузка участников…</p>}
            {teamMembers.map(member => <article className="corp-admin-row" key={member.id}><div className="corp-admin-row-main"><strong title={member.name || member.id}>{member.name || member.id}</strong><small className="corp-admin-id" title={member.id}>{member.id}</small></div><div className="corp-admin-row-actions"><button type="button" onClick={() => removeMember(member)} disabled={busy || !!team.archived_at}>Убрать</button></div></article>)}
            {!membersLoading && !membersError && !teamMembers.length && <p className="corp-admin-empty">В команде пока нет участников. Выберите пользователя выше и нажмите «Добавить».</p>}
          </section>}
        </>}
        {tab === 'projects' && <>
          <form className="corp-admin-create" onSubmit={submit(() => void create())}><input aria-label="Название проекта" value={name} onChange={e => setName(e.target.value)} placeholder="Название проекта"/><input aria-label="Описание проекта" value={description} onChange={e => setDescription(e.target.value)} placeholder="Описание (необязательно)"/><select aria-label="Пространство проекта" value={projectSpace} onChange={e => { setProjectSpace(e.target.value); setProjectParent(''); }}><option value="">Пространство…</option>{spaces.map(s => <option key={s.id} value={s.id}>{s.name}</option>)}</select><select value={projectParent} onChange={e => setProjectParent(e.target.value)} aria-label="Родительский проект"><option value="">Без родительского проекта</option>{projects.filter(p => p.space_id === projectSpace && !p.archived_at).map(p => <option key={p.id} value={p.id}>{p.name}</option>)}</select><button type="submit" disabled={busy || !name.trim() || !projectSpace}><Plus size={15}/>Создать</button></form>
          <div className="corp-admin-catalog-tools"><select value={projectSpaceFilter} onChange={e => setProjectSpaceFilter(e.target.value)} aria-label="Показывать проекты пространства"><option value="">Все пространства</option>{spaces.map(s => <option key={s.id} value={s.id}>{s.name}</option>)}</select></div>
          {visibleProjects.map(projectRow)}
          {!visibleProjects.length && !busy && <p className="corp-admin-empty">Проектов не найдено.</p>}
          {selectedProjectEntry && !canManage(selectedProjectEntry.effective_role) && <p className="corp-admin-empty">Управление доступом к «{selectedProjectEntry.name}» доступно менеджерам и владельцам.</p>}
          {accessAllowed && accessType === 'project' && grantEditor(!!selectedProjectEntry?.archived_at)}
        </>}
        {tab === 'users' && users.map(user => {
          const expanded = userAccess?.userId === user.id;
          return <div className="corp-admin-user" key={user.id}>
            <article className="corp-admin-row">
              <div className="corp-admin-row-main"><strong title={user.name || user.id}>{user.name || user.id}</strong><small className="corp-admin-id" title={user.id}>{user.id}{user.admin ? ' · Администратор' : ''}{user.disabled ? ' · Отключён' : ''}</small></div>
              <div className="corp-admin-row-actions">
                <button type="button" onClick={() => void toggleUserAccess(user)} aria-expanded={expanded} aria-controls={`user-access-${user.id}`}>{expanded ? <ChevronDown size={14}/> : <ChevronRight size={14}/>}Пространства и проекты</button>
                <button type="button" onClick={() => void run(async () => { await client.setUserDisabled(user.id, !user.disabled); userAccessSeq.current++; setUserAccess(null); })} disabled={busy || user.id === userId} title={user.id === userId ? 'Нельзя отключить себя' : undefined}><Shield size={14}/>{user.disabled ? 'Включить' : 'Отключить'}</button>
              </div>
            </article>
            {userAccessPanel(user)}
          </div>;
        })}
        {tab === 'users' && !users.length && <p className="corp-admin-empty">{busy ? 'Загрузка…' : 'Пользователи не найдены или список недоступен.'}</p>}
      </main>
    </section>
  </div>;
}
