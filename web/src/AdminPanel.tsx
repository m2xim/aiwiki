import { useEffect, useState } from 'react';
import { Archive, ArrowLeft, Plus, RefreshCw, Shield } from 'lucide-react';
import { AccessGrant, CatalogSpace, client, Project, Team, TeamMember } from './api';
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

export function AdminPanel({ onClose, userId, isAdmin, activeTab, onTabChange }: { onClose: () => void; userId: string; isAdmin:boolean; activeTab: AdminTab; onTabChange: (tab: AdminTab) => void }) {
  const tab = !isAdmin && (activeTab === 'teams' || activeTab === 'users') ? 'catalog' : activeTab;
  const setTab = onTabChange;
  const [spaces, setSpaces] = useState<CatalogSpace[]>([]);
  const [projects, setProjects] = useState<Project[]>([]);
  const [teams, setTeams] = useState<Team[]>([]);
  const [users, setUsers] = useState<UserRow[]>([]);
  const [directory, setDirectory] = useState<TeamMember[]>([]);
  const [teamMembers, setTeamMembers] = useState<TeamMember[]>([]);
  const [grants, setGrants] = useState<AccessGrant[]>([]);
  const [selectedTeam, setSelectedTeam] = useState('');
  const [selectedSpace, setSelectedSpace] = useState('');
  const [selectedProject, setSelectedProject] = useState('');
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [personal, setPersonal] = useState(true);
  const [projectSpace, setProjectSpace] = useState(''); const [projectParent,setProjectParent]=useState('');
  const [targetId, setTargetId] = useState('');
  const [subjectType, setSubjectType] = useState<'user' | 'team'>('user');
  const [role, setRole] = useState<Role>('reader');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [catalogFilter, setCatalogFilter] = useState('');
  const [catalogSort, setCatalogSort] = useState<'recent' | 'name' | 'count'>('recent');

  const refresh = async () => {
    setBusy(true); setError('');
    try {
      const results = await Promise.allSettled([client.catalog(), tab === 'teams' ? client.teams() : Promise.resolve(null), tab === 'users' ? client.adminUsers() : Promise.resolve(null), client.projects()]);
      const errors: string[] = [];
      const [catalogResult, teamsResult, usersResult, projectsResult] = results;
      if (catalogResult.status === 'fulfilled') { const visibleSpaces = catalogResult.value || []; setSpaces(visibleSpaces); setProjects(visibleSpaces.flatMap(space => space.projects || [])); setSelectedSpace(current => current || visibleSpaces[0]?.id || ''); setProjectSpace(current => current || visibleSpaces[0]?.id || ''); }
      else errors.push(`Каталог: ${catalogResult.reason.message}`);
      if (teamsResult.status === 'fulfilled' && teamsResult.value) { const loadedTeams=teamsResult.value; setTeams(loadedTeams); setSelectedTeam(current => current || loadedTeams[0]?.id || ''); }
      else if (teamsResult.status === 'rejected') errors.push(`Команды: ${teamsResult.reason.message}`);
      if (usersResult.status === 'fulfilled' && usersResult.value) setUsers(usersResult.value);
      else if (usersResult.status === 'rejected') errors.push(`Пользователи: ${usersResult.reason.message}`);
      if (projectsResult.status === 'fulfilled') {
        const catalogProjects = new Map((catalogResult.status === 'fulfilled' ? catalogResult.value.flatMap(space => space.projects || []) : []).map(project => [project.id, project]));
        setProjects(projectsResult.value.map(project => ({ ...catalogProjects.get(project.id), ...project, effective_role: project.effective_role || catalogProjects.get(project.id)?.effective_role, article_count: project.article_count ?? catalogProjects.get(project.id)?.article_count, last_updated: project.last_updated || catalogProjects.get(project.id)?.last_updated, tags: project.tags || catalogProjects.get(project.id)?.tags })));
      }
      else if (catalogResult.status !== 'fulfilled') errors.push(`Проекты: ${projectsResult.reason.message}`);
      setError(errors.join(' · '));
    } finally { setBusy(false); }
  };
  useEffect(() => { if (tab !== 'tokens') void refresh(); }, [tab]);
  useEffect(() => { if (!selectedTeam) { setTeamMembers([]); return; } client.teamMembers(selectedTeam).then(setTeamMembers).catch(e => setError(`Участники команды: ${(e as Error).message}`)); }, [selectedTeam]);
  useEffect(() => {
    const resourceType = tab === 'catalog' ? 'space' : tab === 'projects' ? 'project' : '';
    const resourceId = tab === 'catalog' ? selectedSpace : selectedProject;
    if (!resourceType || !resourceId) { setGrants([]); return; }
    client.access(resourceType, resourceId).then(setGrants).catch(e => setError(`Список доступа: ${(e as Error).message}`));
    client.users(resourceType, resourceId).then(setDirectory).catch(e => { setDirectory([]); setError(`Список пользователей для выдачи доступа: ${(e as Error).message}`); });
  }, [tab, selectedSpace, selectedProject]);
  const run = async (action: () => Promise<unknown>) => { setBusy(true); setError(''); try { await action(); await refresh(); } catch (e) { setError((e as Error).message); } finally { setBusy(false); } };
  const create = async () => {
    if (!name.trim()) return;
    await run(async () => {
      if (tab === 'teams') await client.createTeam(name.trim());
      else if (tab === 'catalog') await client.createSpaceFor({ name: name.trim(), description: description.trim(), personal });
      else if (projectSpace) await client.createProject({ name: name.trim(), description: description.trim(), space_id: projectSpace, parent_id:projectParent, inherit_acl: true });
      else throw new Error('Сначала выберите пространство для проекта.');
      setName(''); setDescription('');
    });
  };
  const addGrant = async () => {
    if (!targetId.trim()) return;
    const isProject = tab === 'projects';
    const resourceId = isProject ? selectedProject : selectedSpace;
    if (!resourceId) return;
    await run(async () => {
      await client.setAccess(isProject ? 'project' : 'space', resourceId, { subject_type: subjectType, subject_id: targetId.trim(), role });
      setTargetId('');
      setGrants(await client.access(isProject ? 'project' : 'space', resourceId));
    });
  };
  const grantEditor = (isProject: boolean) => <>
    <div className="corp-admin-grant"><select value={subjectType} onChange={e => { setSubjectType(e.target.value as 'user' | 'team'); setTargetId(''); }}><option value="user">Пользователь</option><option value="team">Команда</option></select>{subjectType === 'user' ? <select value={targetId} onChange={e => setTargetId(e.target.value)}><option value="">Пользователь…</option>{directory.map(user => <option key={user.id} value={user.id}>{user.name} · {user.id}</option>)}</select> : <input value={targetId} onChange={e => setTargetId(e.target.value)} placeholder="ID команды"/>}<select value={role} onChange={e => setRole(e.target.value as Role)}><option value="reader">Читатель</option><option value="commenter">Комментатор</option><option value="editor">Редактор</option><option value="manager">Менеджер</option><option value="owner">Владелец</option></select><button onClick={() => void addGrant()} disabled={busy || !targetId.trim() || (isProject ? !selectedProject : !selectedSpace)}>Выдать роль</button></div>
    {grants.map(grant => <article key={`${grant.subject_type}:${grant.subject_id}`}><div><strong>{grant.subject_type === 'team' ? 'Команда' : 'Пользователь'} · {grant.subject_id}</strong><small>{grant.role}</small></div><button onClick={() => void run(async () => { await client.removeAccess(isProject ? 'project' : 'space', isProject ? selectedProject : selectedSpace, grant); setGrants(await client.access(isProject ? 'project' : 'space', isProject ? selectedProject : selectedSpace)); })} disabled={busy}>Убрать доступ</button></article>)}
  </>;

  const canManageSpace = (space?: CatalogSpace) => ['manager', 'owner'].includes(space?.effective_role || '');
  const canManageProject = (project?: Project) => ['manager', 'owner'].includes(project?.effective_role || '');
  const catalogSpaces = spaces.filter(space => {
    const needle = catalogFilter.trim().toLocaleLowerCase();
    if (!needle) return true;
    return [space.name, ...(space.projects || []).flatMap(project => [project.name, ...(project.tags || [])]), ...(space.tags || [])].join(' ').toLocaleLowerCase().includes(needle);
  }).sort((a, b) => catalogSort === 'name'
    ? a.name.localeCompare(b.name, 'ru')
    : catalogSort === 'count'
      ? (b.article_count ?? 0) - (a.article_count ?? 0) || a.name.localeCompare(b.name, 'ru')
      : (Date.parse(b.last_updated || '') || 0) - (Date.parse(a.last_updated || '') || 0) || a.name.localeCompare(b.name, 'ru'));

  return <div className="corp-admin-page">
    <section className="corp-admin" aria-labelledby="corp-admin-title">
      <header><div><span>AIWIKI · УПРАВЛЕНИЕ</span><h1 id="corp-admin-title">Команды и доступ</h1></div><button onClick={onClose}><ArrowLeft size={18}/>К базе знаний</button></header>
      <nav>{([['catalog', 'Пространства'], ['teams', 'Команды'], ['projects', 'Проекты'], ['users', 'Пользователи'], ['tokens', 'MCP-доступ']] as [Tab, string][]).filter(([key])=>isAdmin||key==='catalog'||key==='projects'||key==='tokens').map(([key, label]) => <button key={key} className={tab === key ? 'active' : ''} onClick={() => setTab(key)}>{label}</button>)}{tab !== 'tokens' && <button className="corp-admin-refresh" onClick={() => void refresh()} disabled={busy} aria-label="Обновить"><RefreshCw size={15}/></button>}</nav>
      {error && <div className="corp-admin-error" role="alert">{error}</div>}
      <main aria-busy={busy}>
        {tab === 'tokens' && <><MCPPersistentAccess/><MCPDelegations/></>}
        {tab === 'catalog' && <>
          <div className="corp-admin-create"><input value={name} onChange={e => setName(e.target.value)} placeholder="Название пространства"/><input value={description} onChange={e => setDescription(e.target.value)} placeholder="Описание (необязательно)"/><select value={personal ? 'personal' : 'team'} onChange={e => setPersonal(e.target.value === 'personal')}><option value="personal">Личное</option>{isAdmin && <option value="team">Командное</option>}</select><button onClick={() => void create()} disabled={busy || !name.trim()}><Plus size={15}/>Создать</button></div>
          <div className="corp-admin-catalog-tools"><input value={catalogFilter} onChange={e => setCatalogFilter(e.target.value)} placeholder="Поиск по пространствам, проектам и тегам" aria-label="Фильтр каталога"/><select value={catalogSort} onChange={e => setCatalogSort(e.target.value as typeof catalogSort)} aria-label="Сортировка каталога"><option value="recent">Сначала недавно изменённые</option><option value="name">По названию</option><option value="count">По числу документов</option></select></div>
          {catalogSpaces.map(space => <article className="corp-admin-catalog-entry" key={space.id}><button className="corp-admin-project" onClick={() => setSelectedSpace(space.id)} aria-pressed={selectedSpace === space.id}><strong>{space.name}</strong><small>{space.limited ? 'Ограниченный доступ' : space.personal ? 'Личное' : 'Командное'} · {space.article_count ?? 0} документов · {space.projects?.length ?? 0} проектов{space.last_updated ? ` · Изменено ${new Date(space.last_updated).toLocaleDateString()}` : ''}</small><span className="corp-admin-tags">{(space.tags || []).map(tag => <span key={tag}>{tag}</span>)}</span></button>{(space.projects || []).length > 0 && <div className="corp-admin-child-projects"><small>Проекты</small>{(space.projects || []).map(project => <button key={project.id} onClick={() => { setTab('projects'); setSelectedProject(project.id); }}><span>{project.name}</span><small>{project.article_count ?? 0} документов{project.last_updated ? ` · ${new Date(project.last_updated).toLocaleDateString()}` : ''}</small><span className="corp-admin-tags">{(project.tags || []).map(tag => <span key={tag}>{tag}</span>)}</span></button>)}</div>}</article>)}
          {!catalogSpaces.length && <p className="corp-admin-empty">Подходящих пространств не найдено.</p>}
          {selectedSpace && canManageSpace(spaces.find(s => s.id === selectedSpace)) && !spaces.find(s=>s.id===selectedSpace)?.limited && grantEditor(false)}
        </>}
        {tab === 'teams' && <>
          <div className="corp-admin-create"><input value={name} onChange={e => setName(e.target.value)} placeholder="Название команды"/><button onClick={() => void create()} disabled={busy || !name.trim()}><Plus size={15}/>Создать команду</button></div>
          <label className="corp-admin-select">Команда<select value={selectedTeam} onChange={e => setSelectedTeam(e.target.value)}>{teams.map(t => <option key={t.id} value={t.id}>{t.name}</option>)}</select></label>
          {selectedTeam && <div className="corp-admin-grant"><input value={targetId} onChange={e => setTargetId(e.target.value)} placeholder="ID пользователя"/><button onClick={() => void run(async () => { await client.setTeamMember(selectedTeam, targetId.trim()); setTargetId(''); setTeamMembers(await client.teamMembers(selectedTeam)); })} disabled={busy || !targetId.trim()}>Добавить</button></div>}
          {teamMembers.map(member => <article key={member.id}><div><strong>{member.name || member.id}</strong><small>{member.id}</small></div><button onClick={() => void run(async () => { await client.removeTeamMember(selectedTeam, member.id); setTeamMembers(await client.teamMembers(selectedTeam)); })} disabled={busy}>Убрать</button></article>)}
          {!teamMembers.length && selectedTeam && <p className="corp-admin-empty">Список участников пуст или недоступен.</p>}
        </>}
        {tab === 'projects' && <>
          <div className="corp-admin-create"><input value={name} onChange={e => setName(e.target.value)} placeholder="Название проекта"/><select value={projectSpace} onChange={e => {setProjectSpace(e.target.value);setProjectParent('');}}><option value="">Пространство…</option>{spaces.map(s => <option key={s.id} value={s.id}>{s.name}</option>)}</select><select value={projectParent} onChange={e=>setProjectParent(e.target.value)} aria-label="Родительский проект"><option value="">Без родительского проекта</option>{projects.filter(p=>p.space_id===projectSpace&&!p.archived_at).map(p=><option key={p.id} value={p.id}>{p.name}</option>)}</select><button onClick={() => void create()} disabled={busy || !name.trim() || !projectSpace}><Plus size={15}/>Создать</button></div>
          {projects.map(project => <div className="corp-admin-project-row" key={project.id}><button className="corp-admin-project" onClick={() => setSelectedProject(project.id)} aria-pressed={selectedProject === project.id}><strong>{project.name}</strong><small>{project.archived_at ? 'В архиве' : 'Активен'} · {project.inherit_acl ? 'Наследует доступ' : 'Свой список доступа'}</small></button><button title={project.archived_at ? 'Восстановить проект' : 'Архивировать проект'} onClick={() => void run(() => client.updateProject(project.id, { archived: !project.archived_at }))}><Archive size={15}/></button><button title="Переименовать" onClick={() => { const next = window.prompt('Новое название проекта', project.name); if (next?.trim()) void run(() => client.updateProject(project.id, { name: next.trim() })); }}>Изменить</button><label><input type="checkbox" checked={project.inherit_acl} onChange={e => void run(() => client.updateProject(project.id, { inherit_acl: e.target.checked }))}/>Наследовать доступ</label></div>)}
          {selectedProject && canManageProject(projects.find(project => project.id === selectedProject)) && grantEditor(true)}
        </>}
        {tab === 'users' && users.map(user => <article key={user.id}><div><strong>{user.name || user.id}</strong><small>{user.id}{user.admin ? ' · Администратор' : ''}{user.disabled ? ' · Отключён' : ''}</small></div><button onClick={() => void run(() => client.setUserDisabled(user.id, !user.disabled))} disabled={busy}><Shield size={14}/>{user.disabled ? 'Включить' : 'Отключить'}</button></article>)}
        {tab === 'users' && !users.length && <p className="corp-admin-empty">Пользователи не найдены или список недоступен.</p>}
      </main>
    </section>
  </div>;
}
