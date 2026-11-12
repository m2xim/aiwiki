import { FormEvent, useCallback, useEffect, useRef, useState } from 'react';
import { Pencil, Plus, Trash2 } from 'lucide-react';
import { AgentProject, AgentProjectEffective, AgentProjectMode, AgentProjectRow, AgentProjectVisibility, client, Space } from './api';

const MAX_ROWS = 20;
const ROLE_LABELS: Record<string, string> = { owner: 'Владелец', manager: 'Менеджер', editor: 'Редактор', commenter: 'Комментатор', reader: 'Читатель' };
const EFFECTIVE_LABELS: Record<string, string> = { write: 'агент пишет', comment: 'агент комментирует', read: 'агент читает' };
const VISIBILITY_LABELS: Record<AgentProjectVisibility, string> = { private: 'личный', public: 'публичный' };
export const effectiveLabel = (effective?: AgentProjectEffective) => EFFECTIVE_LABELS[effective || ''] || 'агент не получает доступ';
export const projectErrorText = (message: string) => ({
  'invalid project': 'Некорректный проект: проверьте название (1-120 символов) и строки.',
  'space not visible': 'Одно из пространств вам не видно, его нельзя добавить в проект.',
  'too many spaces': `Слишком много строк: не больше ${MAX_ROWS}.`,
  'access denied': 'Недостаточно прав для изменения этого проекта.',
  'not found': 'Проект не найден: возможно, он удалён или стал личным.',
} as Record<string, string>)[message] || message;

type FormRow = { key: string; row_id?: string; space_id?: string; mode: AgentProjectMode; accessible: boolean; name?: string; my_role?: string; effective?: AgentProjectEffective };
type FormState = { id: string; name: string; description: string; visibility: AgentProjectVisibility; ownerId: string; rows: FormRow[] };

const fromRow = (row: AgentProjectRow): FormRow => ({ key: row.row_id, row_id: row.row_id, space_id: row.space_id, mode: row.mode, accessible: row.accessible, name: row.name, my_role: row.my_role, effective: row.effective });
let rowCounter = 0;

export function AgentProjects({ userId, isAdmin }: { userId: string; isAdmin: boolean }) {
  const [projects, setProjects] = useState<AgentProject[] | null>(null);
  const [spaces, setSpaces] = useState<Space[]>([]);
  const [generalId, setGeneralId] = useState('');
  const [form, setForm] = useState<FormState | null>(null);
  const [confirmDelete, setConfirmDelete] = useState('');
  const [error, setError] = useState('');
  const [formError, setFormError] = useState('');
  const [busy, setBusy] = useState(false);
  const mounted = useRef(false);

  const load = useCallback(async () => {
    setError('');
    try {
      const [list, spaceList] = await Promise.all([client.agentProjects(), client.spaces()]);
      if (!mounted.current) return;
      setProjects(list);
      const visible = spaceList.filter(space => !space.archived_at && !space.limited);
      setSpaces(visible);
      try {
        const general = isAdmin ? (await client.adminSettings()).general_space_id : visible[0] ? (await client.bootstrap(visible[0].id)).general_space_id : '';
        if (mounted.current) setGeneralId(general || '');
      } catch { /* the general space is only a hint in the picker */ }
    } catch (e) { if (mounted.current) { setError(projectErrorText((e as Error).message)); setProjects(current => current ?? []); } }
  }, [isAdmin]);
  useEffect(() => { mounted.current = true; void load(); return () => { mounted.current = false; }; }, [load]);

  const spaceLabel = (space: Space) => `${space.name}${space.id === generalId ? ' (общее)' : ''}${space.personal ? ' · личное' : ''}`;
  const startCreate = () => { setFormError(''); setConfirmDelete(''); setForm({ id: '', name: '', description: '', visibility: 'private', ownerId: userId, rows: [] }); };
  const startEdit = (project: AgentProject) => { setFormError(''); setConfirmDelete(''); setForm({ id: project.id, name: project.name, description: project.description || '', visibility: project.visibility, ownerId: project.owner_id, rows: project.spaces.map(fromRow) }); };
  const updateRow = (key: string, patch: Partial<FormRow>) => setForm(current => current && { ...current, rows: current.rows.map(row => row.key === key ? { ...row, ...patch } : row) });
  const usedSpaces = (state: FormState) => new Set(state.rows.map(row => row.space_id).filter(Boolean));
  const addRow = () => setForm(current => {
    if (!current || current.rows.length >= MAX_ROWS) return current;
    const used = usedSpaces(current);
    const free = spaces.find(space => !used.has(space.id));
    if (!free) return current;
    return { ...current, rows: [...current.rows, { key: `new-${++rowCounter}`, space_id: free.id, mode: 'read', accessible: true, name: free.name, my_role: free.effective_role }] };
  });

  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (!form || busy || !form.name.trim()) return;
    const rows = form.rows.map(row => row.row_id ? { row_id: row.row_id, mode: row.mode } : { space_id: row.space_id as string, mode: row.mode });
    setBusy(true); setFormError('');
    try {
      if (form.id) {
        const owner = form.ownerId === userId;
        await client.updateAgentProject(form.id, { name: form.name.trim(), description: form.description.trim(), ...(owner ? { visibility: form.visibility } : {}), spaces: rows });
      } else await client.createAgentProject({ name: form.name.trim(), description: form.description.trim(), visibility: form.visibility, spaces: rows });
      if (!mounted.current) return;
      setForm(null);
      await load();
    } catch (e) { if (mounted.current) setFormError(projectErrorText((e as Error).message)); }
    finally { if (mounted.current) setBusy(false); }
  };
  const remove = async (id: string) => {
    setBusy(true); setError('');
    try { await client.deleteAgentProject(id); if (!mounted.current) return; setConfirmDelete(''); await load(); }
    catch (e) { if (mounted.current) setError(projectErrorText((e as Error).message)); }
    finally { if (mounted.current) setBusy(false); }
  };

  const rowView = (row: AgentProjectRow) => row.accessible
    ? <li className="corp-admin-agent-row" key={row.row_id}>
      <span className="corp-admin-agent-space">{row.name}{row.space_id === generalId && <span className="corp-admin-badge">общее</span>}</span>
      <span className="corp-admin-badge">{row.mode === 'write' ? 'запись' : 'чтение'}</span>
      <small>Ваша роль: {ROLE_LABELS[row.my_role || ''] || 'нет'} · {effectiveLabel(row.effective)}</small>
    </li>
    : <li className="corp-admin-agent-row is-muted" key={row.row_id}>
      <span className="corp-admin-agent-space">Недоступное пространство</span>
      <span className="corp-admin-badge">{row.mode === 'write' ? 'запись' : 'чтение'}</span>
    </li>;

  const formView = (state: FormState) => {
    const owner = state.ownerId === userId;
    const used = usedSpaces(state);
    return <form className="corp-admin-edit corp-admin-agent-form" onSubmit={event => void save(event)}>
      <h3>{state.id ? 'Изменить проект' : 'Новый проект'}</h3>
      <label>Название<input autoFocus maxLength={120} value={state.name} onChange={e => setForm({ ...state, name: e.target.value })} disabled={busy} required/></label>
      <label>Описание<input maxLength={2000} value={state.description} onChange={e => setForm({ ...state, description: e.target.value })} disabled={busy}/></label>
      <label>Видимость
        <select value={state.visibility} onChange={e => setForm({ ...state, visibility: e.target.value as AgentProjectVisibility })} disabled={busy || !owner} title={owner ? undefined : 'Видимость меняет только владелец'}>
          <option value="private">Личный: виден только вам</option><option value="public">Публичный: виден всем, правят владелец и администраторы</option>
        </select>
        {!owner && <small>Видимость меняет только владелец проекта.</small>}
      </label>
      <fieldset className="corp-admin-agent-rows">
        <legend>Пространства <span>Строк: {state.rows.length} из {MAX_ROWS}</span></legend>
        {state.rows.map(row => row.accessible
          ? <div className="corp-admin-agent-edit-row" key={row.key}>
            <select aria-label="Пространство" value={row.space_id} disabled={busy || Boolean(row.row_id)} onChange={e => { const space = spaces.find(item => item.id === e.target.value); updateRow(row.key, { space_id: e.target.value, name: space?.name, my_role: space?.effective_role }); }}>
              {!spaces.some(space => space.id === row.space_id) && <option value={row.space_id}>{row.name || row.space_id}</option>}
              {spaces.filter(space => space.id === row.space_id || !used.has(space.id)).map(space => <option key={space.id} value={space.id}>{spaceLabel(space)}</option>)}
            </select>
            <select aria-label="Режим" value={row.mode} disabled={busy} onChange={e => updateRow(row.key, { mode: e.target.value as AgentProjectMode })}><option value="read">Чтение</option><option value="write">Запись</option></select>
            <button type="button" aria-label="Убрать строку" disabled={busy} onClick={() => setForm({ ...state, rows: state.rows.filter(item => item.key !== row.key) })}><Trash2 size={14}/></button>
            <small>{row.my_role ? `Ваша роль: ${ROLE_LABELS[row.my_role] || row.my_role}` : ''}{row.row_id && row.effective !== undefined ? ` · ${effectiveLabel(row.effective)}` : ''}</small>
          </div>
          : <div className="corp-admin-agent-edit-row" key={row.key}>
            <span className="corp-admin-agent-space is-muted">Недоступное пространство</span>
            <select aria-label="Режим" value={row.mode} disabled={busy} onChange={e => updateRow(row.key, { mode: e.target.value as AgentProjectMode })}><option value="read">Чтение</option><option value="write">Запись</option></select>
            <button type="button" aria-label="Убрать строку" disabled={busy} onClick={() => setForm({ ...state, rows: state.rows.filter(item => item.key !== row.key) })}><Trash2 size={14}/></button>
          </div>)}
        {!state.rows.length && <p className="corp-admin-empty">Строк пока нет: агент в таком проекте читает только общее пространство.</p>}
        <button type="button" disabled={busy || state.rows.length >= MAX_ROWS || spaces.every(space => used.has(space.id))} onClick={addRow}><Plus size={14}/>Добавить пространство</button>
      </fieldset>
      {formError && <p className="corp-admin-inline-error" role="alert">{formError}</p>}
      <div className="corp-admin-row-actions"><button type="submit" disabled={busy || !state.name.trim()}>Сохранить</button><button type="button" disabled={busy} onClick={() => { setForm(null); setFormError(''); }}>Отмена</button></div>
    </form>;
  };

  return <section className="corp-admin-agent-projects" aria-label="Проекты агентов" aria-busy={busy}>
    <p className="corp-admin-hint">Проект не даёт прав — он ограничивает агента. Агент получает меньшее из режима в проекте и ваших собственных прав в пространстве. Общее пространство агент читает в любом проекте; чтобы он мог писать в общее, добавьте его строкой с режимом «Запись».</p>
    {error && <div className="corp-admin-error" role="alert">{error}</div>}
    {!form && <div className="corp-admin-create"><button type="button" onClick={startCreate} disabled={busy}><Plus size={15}/>Создать проект</button></div>}
    {form && !form.id && formView(form)}
    {projects === null && <p className="corp-admin-empty">Загрузка…</p>}
    {projects?.length === 0 && !form && <p className="corp-admin-empty">Проектов пока нет. Создайте первый.</p>}
    {projects?.map(project => {
      if (form?.id === project.id) return <div className="corp-admin-project-row" key={project.id}>{formView(form)}</div>;
      const mine = project.owner_id === userId;
      return <article className="corp-admin-project-row corp-admin-agent-project" key={project.id}>
        <div className="corp-admin-row-main">
          <strong title={project.name}>{project.name}</strong>
          <span className="corp-admin-badges">
            <span className="corp-admin-badge">{VISIBILITY_LABELS[project.visibility]}</span>
            <span className="corp-admin-badge is-muted">{mine ? 'ваш' : `владелец: ${project.owner_name || project.owner_id}`}</span>
          </span>
          {project.description && <small className="corp-admin-desc">{project.description}</small>}
          {project.spaces.length > 0 ? <ul className="corp-admin-agent-list">{project.spaces.map(rowView)}</ul> : <small className="corp-admin-desc">Пространств нет: агент читает только общее.</small>}
        </div>
        {project.can_edit && <div className="corp-admin-row-actions">
          {confirmDelete === project.id
            ? <><span>Удалить проект? Ключи перестанут его получать.</span><button type="button" disabled={busy} onClick={() => void remove(project.id)}>Удалить</button><button type="button" disabled={busy} onClick={() => setConfirmDelete('')}>Отмена</button></>
            : <><button type="button" disabled={busy || Boolean(form)} onClick={() => startEdit(project)}><Pencil size={14}/>Изменить</button><button type="button" disabled={busy} onClick={() => { setForm(null); setConfirmDelete(project.id); }}><Trash2 size={14}/>Удалить</button></>}
        </div>}
      </article>;
    })}
  </section>;
}
