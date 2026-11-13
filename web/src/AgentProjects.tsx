import { useCallback, useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from 'react';
import { Eye, Pencil, Plus, RefreshCw, Trash2, X } from 'lucide-react';
import { type AgentProject, type AgentProjectEffective, type AgentProjectMode, type AgentProjectRow, type AgentProjectVisibility, client, type Space } from './api';
import './AdminSpaceDialogs.css';
import './AgentProjects.css';

const MAX_ROWS = 20;
const ROLE_LABELS: Record<string, string> = { owner: 'Владелец', manager: 'Менеджер', editor: 'Редактор', commenter: 'Комментатор', reader: 'Читатель' };
const EFFECTIVE_LABELS: Record<string, string> = { write: 'агент пишет', comment: 'агент комментирует', read: 'агент читает' };
const VISIBILITY_LABELS: Record<AgentProjectVisibility, string> = { private: 'Личный', public: 'Публичный' };
export const effectiveLabel = (effective?: AgentProjectEffective) => EFFECTIVE_LABELS[effective || ''] || 'агент не получает доступ';
export const projectErrorText = (message: string) => ({
  'invalid project': 'Некорректный проект: проверьте название (1-120 символов) и строки.',
  'space not visible': 'Одно из пространств вам не видно, его нельзя добавить в проект.',
  'too many spaces': `Слишком много строк: не больше ${MAX_ROWS}.`,
  'access denied': 'Недостаточно прав для изменения этого проекта.',
  'not found': 'Проект не найден: возможно, он удалён или стал личным.',
} as Record<string, string>)[message] || message;
const errorText = (error: unknown) => projectErrorText(error instanceof Error ? error.message : String(error));
const matches = (query: string, ...values: (string | undefined)[]) => values.some(value => value?.toLocaleLowerCase().includes(query.trim().toLocaleLowerCase()));
type FormRow = { key: string; row_id?: string; space_id?: string; mode: AgentProjectMode; accessible: boolean; name?: string; my_role?: string; effective?: AgentProjectEffective; savedMode?: AgentProjectMode };
const fromRow = (row: AgentProjectRow): FormRow => ({ ...row, key: row.row_id, savedMode: row.mode });
const HELP = 'Агент получает меньшее из режима в проекте и ваших прав в пространстве. Общее пространство всегда доступно для чтения. Для записи добавьте его в проект с режимом «Запись».';

function ProjectDialog({ title, busy, onClose, children, small = false }: { title: string; busy: boolean; onClose: () => void; children: ReactNode; small?: boolean }) {
  const ref = useRef<HTMLDialogElement>(null);
  const id = useId();
  useEffect(() => {
    const dialog = ref.current;
    const opener = document.activeElement;
    dialog?.showModal(); dialog?.querySelector<HTMLElement>('[data-initial-focus]')?.focus();
    return () => { dialog?.close(); if (opener instanceof HTMLElement && opener.isConnected) opener.focus({ preventScroll: true }); };
  }, []);
  return <dialog ref={ref} className={`spaces-dialog admin-space-dialog agent-project-dialog${small ? ' agent-project-dialog--small' : ''}`} aria-labelledby={id}
    onCancel={event => { event.preventDefault(); event.stopPropagation(); if (!busy) onClose(); }}>
    <header className="spaces-head"><h2 id={id}>{title}</h2><button className="icon-button" type="button" aria-label={`Закрыть: ${title}`} disabled={busy} onClick={onClose}><X size={20}/></button></header>
    {children}
  </dialog>;
}

export function AgentProjects({ userId, isAdmin }: { userId: string; isAdmin: boolean }) {
  const [projects, setProjects] = useState<AgentProject[]>([]);
  const [spaces, setSpaces] = useState<Space[]>([]);
  const [generalId, setGeneralId] = useState('');
  const [editing, setEditing] = useState<AgentProject | 'new' | null>(null);
  const [deleting, setDeleting] = useState<AgentProject | null>(null);
  const [search, setSearch] = useState('');
  const [visibility, setVisibility] = useState('all');
  const [sort, setSort] = useState('name');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const sequence = useRef(0);
  const load = useCallback(async () => {
    const seq = ++sequence.current; setLoading(true); setError('');
    try {
      const [list, spaceList] = await Promise.all([client.agentProjects(), client.spaces()]);
      if (seq !== sequence.current) return;
      setProjects(list); const visible = spaceList.filter(space => !space.archived_at && !space.limited); setSpaces(visible);
      try {
        const general = isAdmin ? (await client.adminSettings()).general_space_id : visible[0] ? (await client.bootstrap(visible[0].id)).general_space_id : '';
        if (seq === sequence.current) setGeneralId(general || '');
      } catch { /* General space is an optional picker label. */ }
    } catch (cause) { if (seq === sequence.current) setError(errorText(cause)); }
    finally { if (seq === sequence.current) setLoading(false); }
  }, [isAdmin]);
  useEffect(() => { void load(); return () => { sequence.current++; }; }, [load]);
  const filtered = projects.filter(project => (visibility === 'all' || project.visibility === visibility) && matches(search, project.name, project.description, project.owner_name, project.owner_id))
    .sort((a, b) => sort === 'updated' ? b.updated_at.localeCompare(a.updated_at) : a.name.localeCompare(b.name, 'ru'));
  return <>
    <section className="admin-work-panel agent-project-panel" aria-label="Проекты агентов" aria-busy={loading}>
      <div className="admin-work-toolbar agent-project-toolbar">
        <button className="button primary" type="button" onClick={() => setEditing('new')} disabled={loading}><Plus size={16}/>Создать проект</button>
        <input type="search" aria-label="Поиск по проектам" placeholder="Название, описание или владелец" value={search} onChange={event => setSearch(event.target.value)}/>
        <select aria-label="Видимость проектов" value={visibility} onChange={event => setVisibility(event.target.value)}><option value="all">Вся видимость</option><option value="private">Личные</option><option value="public">Публичные</option></select>
        <select aria-label="Сортировка проектов" value={sort} onChange={event => setSort(event.target.value)}><option value="name">По названию</option><option value="updated">По обновлению</option></select>
        <button className="icon-button" type="button" title="Обновить проекты" aria-label="Обновить проекты" disabled={loading} onClick={() => void load()}><RefreshCw size={17}/></button>
      </div>
      {error && <p className="corp-admin-error" role="alert">{error}</p>}
      <div className="admin-work-table-wrap"><table className="admin-work-table agent-project-table" aria-label="Список проектов агентов">
        <colgroup><col/><col className="agent-project-visibility-col"/><col className="agent-project-owner-col"/><col className="agent-project-count-col"/><col className="agent-project-actions-col"/></colgroup>
        <thead><tr><th scope="col">Проект</th><th scope="col">Видимость</th><th scope="col">Владелец</th><th scope="col">Пространства</th><th scope="col">Действия</th></tr></thead>
        <tbody>{filtered.map(project => <tr key={project.id}>
          <th scope="row"><strong>{project.name}</strong>{project.description && <small className="corp-admin-desc agent-project-description">{project.description}</small>}</th>
          <td><span className="corp-admin-badge">{VISIBILITY_LABELS[project.visibility]}</span></td>
          <td><span className="agent-project-owner" title={project.owner_name || project.owner_id}>{project.owner_id === userId ? 'Вы' : project.owner_name || project.owner_id}</span></td>
          <td>{project.spaces.length}</td><td><div className="admin-space-row-actions">
            <button className="icon-button" type="button" title={project.can_edit ? 'Редактировать проект' : 'Просмотреть проект'} aria-label={`${project.can_edit ? 'Изменить' : 'Просмотреть'} проект «${project.name}»`} onClick={() => setEditing(project)}>{project.can_edit ? <Pencil size={17}/> : <Eye size={17}/>}</button>
            {project.can_edit && <button className="icon-button" type="button" title="Удалить проект" aria-label={`Удалить проект «${project.name}»`} onClick={() => setDeleting(project)}><Trash2 size={17}/></button>}
          </div></td>
        </tr>)}</tbody>
      </table></div>
      {!filtered.length && <p className="corp-admin-empty" role="status">{loading ? 'Загрузка проектов…' : projects.length ? 'Проекты по этим фильтрам не найдены.' : 'Проектов пока нет. Создайте первый.'}</p>}
      <details className="admin-work-help"><summary>Как работает доступ агента</summary><p>{HELP}</p></details>
    </section>
    {editing && <ProjectEditor key={editing === 'new' ? 'new' : editing.id} project={editing === 'new' ? undefined : editing} userId={userId} spaces={spaces} generalId={generalId} onChanged={() => void load()} onClose={() => setEditing(null)}/>}
    {deleting && <DeleteProject project={deleting} onChanged={() => void load()} onClose={() => setDeleting(null)}/>}
  </>;
}

function DeleteProject({ project, onChanged, onClose }: { project: AgentProject; onChanged: () => void; onClose: () => void }) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState(''); const guard = useRef(false);
  async function remove(event: FormEvent) {
    event.preventDefault(); if (guard.current || !project.can_edit) return;
    guard.current = true; setPending(true); setError('');
    try { await client.deleteAgentProject(project.id); onChanged(); onClose(); }
    catch (cause) { setError(errorText(cause)); }
    finally { guard.current = false; setPending(false); }
  }
  return <ProjectDialog title={`Удалить проект «${project.name}»?`} small busy={pending} onClose={onClose}>
    <form className="admin-space-confirm" onSubmit={remove}><p>Проект будет удалён без возможности восстановления. Подключённые ключи перестанут его получать. Пространства и материалы сохранятся.</p>
      {error && <p className="spaces-error" role="alert">{error}</p>}
      <div className="spaces-actions"><button className="button ghost" type="button" data-initial-focus disabled={pending} onClick={onClose}>Нет</button><button className="button primary" type="submit" disabled={pending}>{pending ? 'Удаляем…' : 'Да, удалить'}</button></div>
    </form>
  </ProjectDialog>;
}

function ProjectEditor({ project, userId, spaces, generalId, onChanged, onClose }: { project?: AgentProject; userId: string; spaces: Space[]; generalId: string; onChanged: () => void; onClose: () => void }) {
  const [current, setCurrent] = useState(project);
  const [name, setName] = useState(project?.name || '');
  const [description, setDescription] = useState(project?.description || '');
  const [visibility, setVisibility] = useState<AgentProjectVisibility>(project?.visibility || 'private');
  const [rows, setRows] = useState<FormRow[]>(project?.spaces.map(fromRow) || []);
  const [tab, setTab] = useState<'general' | 'spaces'>('general');
  const [search, setSearch] = useState(''); const [modeFilter, setModeFilter] = useState('all');
  const [adding, setAdding] = useState(false);
  const [pending, setPending] = useState(false); const [error, setError] = useState(''); const [saved, setSaved] = useState('');
  const guard = useRef(false); const counter = useRef(0); const id = useId();
  const editable = !current || current.can_edit;
  const owner = !current || current.owner_id === userId;
  const serializedRows = rows.map(row => row.row_id ? { row_id: row.row_id, mode: row.mode } : { space_id: row.space_id!, mode: row.mode });
  const originalRows = current?.spaces.map(row => ({ row_id: row.row_id, mode: row.mode })) || [];
  const dirty = !current || name.trim() !== current.name || description.trim() !== (current.description || '') || visibility !== current.visibility || JSON.stringify(serializedRows) !== JSON.stringify(originalRows);
  const used = new Set(rows.map(row => row.space_id).filter(Boolean));
  const visibleRows = rows.filter(row => (modeFilter === 'all' || row.mode === modeFilter) && matches(search, row.accessible ? row.name : 'Недоступное пространство'));
  async function save(event: FormEvent) {
    event.preventDefault(); if (!editable || !name.trim() || !dirty || guard.current) return;
    guard.current = true; setPending(true); setError(''); setSaved(''); const creating = !current;
    try {
      const data = { name: name.trim(), description: description.trim(), spaces: serializedRows };
      const updated = current ? await client.updateAgentProject(current.id, { ...data, ...(owner ? { visibility } : {}) }) : await client.createAgentProject({ ...data, visibility });
      setCurrent(updated); setName(updated.name); setDescription(updated.description || ''); setVisibility(updated.visibility); setRows(updated.spaces.map(fromRow));
      setSaved(creating ? 'Проект создан' : 'Изменения сохранены'); onChanged();
    } catch (cause) { setError(errorText(cause)); }
    finally { guard.current = false; setPending(false); }
  }
  return <ProjectDialog title={current ? `Проект «${current.name}»` : 'Создать проект'} busy={pending} onClose={onClose}>
    <div className="admin-space-tabs" role="tablist" aria-label="Настройки проекта">
      {(['general', 'spaces'] as const).map(key => <button key={key} type="button" className="button ghost" role="tab" id={`${id}-${key}`} aria-controls={`${id}-${key}-panel`} aria-selected={tab === key} tabIndex={tab === key ? 0 : -1} disabled={pending} onClick={() => setTab(key)} onKeyDown={event => {
        if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
        event.preventDefault(); const next = event.key === 'Home' ? 'general' : event.key === 'End' ? 'spaces' : tab === 'general' ? 'spaces' : 'general'; setTab(next); document.getElementById(`${id}-${next}`)?.focus();
      }}>{key === 'general' ? 'Общее' : 'Пространства'}</button>)}
    </div>
    <form className="agent-project-editor" onSubmit={save} aria-busy={pending}>
      <div className="agent-project-general" role="tabpanel" id={`${id}-general-panel`} aria-labelledby={`${id}-general`} hidden={tab !== 'general'}>
        <label className="field"><span>Название</span><input data-initial-focus required maxLength={120} value={name} disabled={!editable || pending} onChange={event => { setName(event.target.value); setSaved(''); }}/></label>
        <label className="field"><span>Описание</span><textarea rows={4} maxLength={2000} value={description} disabled={!editable || pending} onChange={event => { setDescription(event.target.value); setSaved(''); }}/></label>
        <label className="field"><span>Видимость</span><select value={visibility} disabled={!editable || !owner || pending} onChange={event => { setVisibility(event.target.value as AgentProjectVisibility); setSaved(''); }}><option value="private">Личный</option><option value="public">Публичный</option></select></label>
        <p className="corp-admin-hint">{visibility === 'private' ? 'Проект виден только владельцу.' : 'Проект виден всем. Изменять его могут владелец и администраторы.'}{!owner && ' Видимость меняет только владелец.'}</p>
      </div>
      <div className="agent-project-spaces" role="tabpanel" id={`${id}-spaces-panel`} aria-labelledby={`${id}-spaces`} hidden={tab !== 'spaces'}>
        <div className="agent-project-space-filters"><label className="field"><span>Поиск пространств</span><input type="search" placeholder="Название пространства" value={search} onChange={event => setSearch(event.target.value)}/></label>
          <label className="field"><span>Режим</span><select value={modeFilter} onChange={event => setModeFilter(event.target.value)}><option value="all">Все режимы</option><option value="read">Чтение</option><option value="write">Запись</option></select></label>
          {editable && <button className="button primary admin-access-add-button" type="button" aria-label="Добавить пространства" title="Добавить пространства" disabled={pending || rows.length >= MAX_ROWS || spaces.every(space => used.has(space.id))} onClick={() => setAdding(true)}><Plus size={20}/></button>}
        </div>
        <p className="admin-access-count">Пространства: {rows.length} из {MAX_ROWS}{visibleRows.length !== rows.length ? ` · Найдено: ${visibleRows.length}` : ''}</p>
        <div className="admin-work-table-wrap"><table className="admin-work-table agent-project-spaces-table" aria-label="Пространства проекта"><colgroup><col/><col className="agent-project-mode-col"/><col className="agent-project-effective-col"/><col className="agent-project-row-action-col"/></colgroup>
          <thead><tr><th scope="col">Пространство</th><th scope="col">Режим</th><th scope="col">Доступ агента</th><th scope="col">Действия</th></tr></thead>
          <tbody>{visibleRows.map(row => <tr key={row.key}><th scope="row"><strong>{row.accessible ? row.name || 'Пространство' : 'Недоступное пространство'}</strong>{row.accessible && row.space_id === generalId && <span className="corp-admin-badge">общее</span>}{row.accessible && row.my_role && <small className="corp-admin-desc">Ваша роль: {ROLE_LABELS[row.my_role] || row.my_role}</small>}</th>
            <td><select aria-label={`Режим: ${row.accessible ? row.name : 'Недоступное пространство'}`} value={row.mode} disabled={!editable || pending} onChange={event => { const mode = event.target.value as AgentProjectMode; setRows(list => list.map(item => item.key === row.key ? { ...item, mode } : item)); setSaved(''); }}><option value="read">Чтение</option><option value="write">Запись</option></select></td>
            <td>{!row.accessible ? 'Нет доступа' : row.savedMode === row.mode && row.effective !== undefined ? effectiveLabel(row.effective) : 'Уточнится после сохранения'}</td>
            <td>{editable && <button className="icon-button" type="button" title="Убрать из проекта" aria-label={`Убрать пространство: ${row.accessible ? row.name : 'Недоступное пространство'}`} disabled={pending} onClick={() => { setRows(list => list.filter(item => item.key !== row.key)); setSaved(''); }}><Trash2 size={17}/></button>}</td></tr>)}</tbody>
        </table></div>
        {!visibleRows.length && <p className="corp-admin-empty">{rows.length ? 'Пространства по этим фильтрам не найдены.' : 'Пространств пока нет. Агент читает только общее пространство.'}</p>}
        <p className="corp-admin-hint">{HELP}</p>
      </div>
      {error && <p className="spaces-error" role="alert">{error}</p>}
      <footer className="spaces-actions agent-project-footer"><span className="agent-project-save-state" role="status">{!editable ? 'Только просмотр' : dirty ? 'Есть несохранённые изменения' : saved}</span>
        <button className="button ghost" type="button" disabled={pending} onClick={onClose}>Закрыть</button>
        {editable && <button className="button primary" type="submit" disabled={pending || !name.trim() || !dirty}>{pending ? 'Сохраняем…' : current ? 'Сохранить' : 'Создать'}</button>}
      </footer>
    </form>
    {adding && editable && <SpacePicker spaces={spaces.filter(space => !used.has(space.id))} generalId={generalId} remaining={MAX_ROWS - rows.length} onClose={() => setAdding(false)} onAdd={(chosen, mode) => {
      setRows(list => [...list, ...chosen.map(space => ({ key: `new-${++counter.current}`, space_id: space.id, mode, accessible: true, name: space.name, my_role: space.effective_role }))]); setSaved(''); setAdding(false);
    }}/>}
  </ProjectDialog>;
}

function SpacePicker({ spaces, generalId, remaining, onAdd, onClose }: { spaces: Space[]; generalId: string; remaining: number; onAdd: (spaces: Space[], mode: AgentProjectMode) => void; onClose: () => void }) {
  const [query, setQuery] = useState(''); const [selected, setSelected] = useState<Set<string>>(() => new Set()); const [mode, setMode] = useState<AgentProjectMode>('read');
  const chosen = spaces.filter(space => selected.has(space.id));
  const visible = spaces.filter(space => matches(query, space.name, space.description));
  return <ProjectDialog title="Добавить пространства" small busy={false} onClose={onClose}>
    <form className="admin-space-access-form agent-project-picker" onSubmit={event => { event.preventDefault(); if (chosen.length && chosen.length <= remaining) onAdd(chosen, mode); }}>
      <p className="admin-access-description">Выберите пространства и общий режим. Изменения применятся после сохранения проекта.</p>
      <label className="field"><span>Найти пространство</span><input data-initial-focus type="search" placeholder="Название или описание" value={query} onChange={event => setQuery(event.target.value)}/></label>
      <div className="admin-access-candidates" role="group" aria-label="Доступные пространства">{visible.map(space => <label key={space.id} className="admin-access-candidate" data-selected={selected.has(space.id)}>
        <input type="checkbox" checked={selected.has(space.id)} disabled={!selected.has(space.id) && chosen.length >= remaining} onChange={event => { const checked = event.target.checked; setSelected(ids => { const next = new Set(ids); if (checked) next.add(space.id); else next.delete(space.id); return next; }); }}/>
        <span><strong>{space.name}</strong><small>{space.id === generalId ? 'Общее пространство' : space.personal ? 'Личное пространство' : 'Командное пространство'}</small></span>
      </label>)}{!visible.length && <p className="corp-admin-empty">Пространства не найдены.</p>}</div>
      <p className="admin-access-selection-count" role="status">Выбрано: {chosen.length} · Можно добавить: {remaining}</p>
      <label className="field"><span>Режим для выбранных пространств</span><select value={mode} onChange={event => setMode(event.target.value as AgentProjectMode)}><option value="read">Чтение</option><option value="write">Запись</option></select></label>
      <div className="spaces-actions admin-access-add-actions"><button className="button ghost" type="button" onClick={onClose}>Отмена</button><button className="button primary" type="submit" disabled={!chosen.length || chosen.length > remaining}>Добавить{chosen.length ? ` (${chosen.length})` : ''}</button></div>
    </form>
  </ProjectDialog>;
}
