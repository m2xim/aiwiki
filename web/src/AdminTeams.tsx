import { useCallback, useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from 'react';
import { Archive, Pencil, Plus, RotateCcw, Trash2, X } from 'lucide-react';
import { client, type Team, type TeamMember } from './api';
import './AdminSpaceDialogs.css';
import './AdminTeams.css';

type User = { id: string; name: string; disabled: boolean; admin: boolean };
type Props = { teams: readonly Team[]; users: readonly User[]; loading: boolean; onChanged: () => void };
const message = (error: unknown) => {
  const text = error instanceof Error ? error.message : String(error);
  return /last active owner/i.test(text)
    ? 'Это действие оставит пространство без активного владельца. Сначала назначьте другого владельца, затем повторите попытку.'
    : text;
};
const match = (query: string, ...values: string[]) => values.some(value => value.toLocaleLowerCase().includes(query.trim().toLocaleLowerCase()));

function TeamDialog({ title, busy, onClose, children, small = false }: {
  title: string; busy: boolean; onClose: () => void; children: ReactNode; small?: boolean;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const id = useId();
  useEffect(() => {
    const dialog = ref.current;
    const opener = document.activeElement;
    dialog?.showModal();
    dialog?.querySelector<HTMLElement>('[data-initial-focus]')?.focus();
    return () => { dialog?.close(); if (opener instanceof HTMLElement && opener.isConnected) opener.focus({ preventScroll: true }); };
  }, []);
  return <dialog ref={ref} aria-labelledby={id} className={`spaces-dialog admin-space-dialog admin-team-dialog${small ? ' admin-team-dialog--small' : ''}`}
    onCancel={event => { event.preventDefault(); event.stopPropagation(); if (!busy) onClose(); }}>
    <header className="spaces-head"><h2 id={id}>{title}</h2><button className="icon-button" type="button" aria-label={`Закрыть: ${title}`} disabled={busy} onClick={onClose}><X size={20}/></button></header>
    {children}
  </dialog>;
}

export function AdminTeams({ teams, users, loading, onChanged }: Props) {
  const [group, setGroup] = useState<'active' | 'archive'>('active');
  const [query, setQuery] = useState('');
  const [sort, setSort] = useState('asc');
  const [editing, setEditing] = useState<Team | null>(null);
  const [creating, setCreating] = useState(false);
  const [archiving, setArchiving] = useState<Team | null>(null);
  const [restoring, setRestoring] = useState('');
  const [error, setError] = useState('');
  const mutation = useRef(false);
  const visible = teams.filter(team => Boolean(team.archived_at) === (group === 'archive') && match(query, team.name, team.description || '', team.id))
    .sort((a, b) => a.name.localeCompare(b.name, 'ru') * (sort === 'asc' ? 1 : -1));
  async function restore(team: Team) {
    if (mutation.current) return;
    mutation.current = true; setRestoring(team.id); setError('');
    try { await client.updateTeam(team.id, { archived: false }); onChanged(); }
    catch (cause) { setError(message(cause)); }
    finally { mutation.current = false; setRestoring(''); }
  }
  return <>
    <section className="admin-work-panel admin-teams-panel" aria-label="Список команд">
      <div className="admin-work-toolbar admin-teams-toolbar">
        <div className="admin-space-catalog-tabs" role="group" aria-label="Состояние команд">
          <button className="button ghost" type="button" aria-pressed={group === 'active'} onClick={() => setGroup('active')}>Активные</button>
          <button className="button ghost" type="button" aria-pressed={group === 'archive'} onClick={() => setGroup('archive')}>Архив</button>
        </div>
        <button className="button primary" type="button" disabled={loading || !!restoring} onClick={() => setCreating(true)}><Plus size={16}/>Создать команду</button>
        <input type="search" aria-label="Поиск по командам" placeholder="Название, описание или ID команды" value={query} onChange={event => setQuery(event.target.value)}/>
        <select aria-label="Сортировка команд" value={sort} onChange={event => setSort(event.target.value)}><option value="asc">По названию А–Я</option><option value="desc">По названию Я–А</option></select>
      </div>
      {error && <p className="corp-admin-inline-error admin-team-notice" role="alert">{error}</p>}
      <div className="admin-work-table-wrap"><table className="admin-work-table admin-teams-table" aria-label="Команды">
        <colgroup><col/><col/><col className="admin-team-status-col"/><col className="admin-team-actions-col"/></colgroup>
        <thead><tr><th scope="col">Название</th><th scope="col">Описание</th><th scope="col">Статус</th><th scope="col">Действия</th></tr></thead>
        <tbody>{visible.map(team => <tr key={team.id}><th scope="row"><strong>{team.name}</strong></th><td>{team.description || '—'}</td>
          <td><span className="corp-admin-badge">{team.archived_at ? 'В архиве' : 'Активна'}</span></td>
          <td><div className="admin-space-row-actions">
            <button className="icon-button" type="button" title="Редактировать команду" aria-label={`Редактировать команду «${team.name}»`} disabled={loading || !!restoring} onClick={() => setEditing(team)}><Pencil size={17}/></button>
            {team.archived_at
              ? <button className="icon-button" type="button" title="Восстановить команду" aria-label={`Восстановить команду «${team.name}»`} disabled={loading || !!restoring} onClick={() => void restore(team)}><RotateCcw size={17}/></button>
              : <button className="icon-button" type="button" title="Архивировать команду" aria-label={`Архивировать команду «${team.name}»`} disabled={loading || !!restoring} onClick={() => setArchiving(team)}><Archive size={17}/></button>}
          </div></td></tr>)}</tbody>
      </table></div>
      {!visible.length && <p className="corp-admin-empty" role="status">{loading ? 'Загрузка команд…' : query ? 'Команды по этому запросу не найдены.' : group === 'archive' ? 'Архивных команд пока нет.' : 'Команд пока нет.'}</p>}
    </section>
    {creating && <TeamEditor users={users} onClose={() => setCreating(false)} onChanged={() => { setGroup('active'); onChanged(); }}/>}
    {editing && <TeamEditor key={editing.id} team={editing} users={users} onClose={() => setEditing(null)} onChanged={onChanged}/>}
    {archiving && <ArchiveTeam team={archiving} onClose={() => setArchiving(null)} onChanged={onChanged}/>}
  </>;
}

function ArchiveTeam({ team, onClose, onChanged }: { team: Team; onClose: () => void; onChanged: () => void }) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState('');
  const guard = useRef(false);
  async function archive(event: FormEvent) {
    event.preventDefault(); if (guard.current) return; guard.current = true; setPending(true); setError('');
    try { await client.updateTeam(team.id, { archived: true }); onChanged(); onClose(); }
    catch (cause) { setError(message(cause)); }
    finally { guard.current = false; setPending(false); }
  }
  return <TeamDialog title={`Архивировать «${team.name}»?`} small busy={pending} onClose={onClose}>
    <form className="admin-space-confirm" onSubmit={archive}>
      <p>Команда исчезнет из активных. Доступ через неё перестанет действовать. Участники сохранятся, команду можно восстановить из архива.</p>
      {error && <p className="spaces-error" role="alert">{error}</p>}
      <div className="spaces-actions"><button className="button ghost" type="button" data-initial-focus disabled={pending} onClick={onClose}>Нет</button>
        <button className="button primary" type="submit" disabled={pending}>{pending ? 'Архивируем…' : 'Да, архивировать'}</button></div>
    </form>
  </TeamDialog>;
}

function TeamEditor({ team, users, onClose, onChanged }: { team?: Team; users: readonly User[]; onClose: () => void; onChanged: () => void }) {
  const [current, setCurrent] = useState(team);
  const [name, setName] = useState(team?.name || '');
  const [description, setDescription] = useState(team?.description || '');
  const [tab, setTab] = useState<'general' | 'members'>('general');
  const [saving, setSaving] = useState(false);
  const [memberPending, setMemberPending] = useState(false);
  const [error, setError] = useState('');
  const [saved, setSaved] = useState(false);
  const guard = useRef(false);
  const id = useId();
  const busy = saving || memberPending;
  const changed = name.trim() !== current?.name || description !== (current?.description || '');
  async function save(event: FormEvent) {
    event.preventDefault(); if (!name.trim() || busy || guard.current || !changed) return;
    guard.current = true; setSaving(true); setError(''); setSaved(false);
    try {
      const updated = current ? await client.updateTeam(current.id, { name: name.trim(), description }) : await client.createTeam(name.trim(), description);
      setCurrent(updated); setName(updated.name); setDescription(updated.description || ''); setSaved(true); onChanged();
    } catch (cause) { setError(message(cause)); }
    finally { guard.current = false; setSaving(false); }
  }
  return <TeamDialog title={current ? `Команда «${current.name}»` : 'Создать команду'} busy={busy} onClose={onClose}>
    <div className="admin-space-tabs" role="tablist" aria-label="Настройки команды">
      {(['general', 'members'] as const).map(key => <button className="button ghost" type="button" role="tab" key={key}
        id={`${id}-${key}`} aria-controls={`${id}-${key}-panel`} aria-selected={tab === key} tabIndex={tab === key ? 0 : -1}
        disabled={busy || key === 'members' && !current} data-initial-focus={key === 'general' && current ? '' : undefined}
        onClick={() => setTab(key)} onKeyDown={event => {
          if (!current || busy || !['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
          event.preventDefault(); const next = event.key === 'Home' ? 'general' : event.key === 'End' ? 'members' : tab === 'general' ? 'members' : 'general';
          setTab(next); document.getElementById(`${id}-${next}`)?.focus();
        }}>{key === 'general' ? 'Общее' : 'Участники'}</button>)}
    </div>
    <div role="tabpanel" id={`${id}-general-panel`} aria-labelledby={`${id}-general`} hidden={tab !== 'general'}>
      <form className="admin-team-general" onSubmit={save}>
        <label className="field"><span>Название команды</span><input required maxLength={300} data-initial-focus={!current ? '' : undefined} value={name} disabled={busy} onChange={event => { setName(event.target.value); setSaved(false); }}/></label>
        <label className="field"><span>Описание</span><textarea rows={4} value={description} disabled={busy} onChange={event => { setDescription(event.target.value); setSaved(false); }}/></label>
        {current?.archived_at && <p className="corp-admin-hint">Команда в архиве. Состав участников можно изменить после восстановления.</p>}
        {error && <p className="spaces-error" role="alert">{error}</p>}
        <div className="spaces-actions">{saved && <span role="status" className="admin-space-saved">Изменения сохранены</span>}
          <button className="button ghost" type="button" disabled={busy} onClick={onClose}>Закрыть</button>
          <button className="button primary" type="submit" disabled={busy || !name.trim() || !changed}>{saving ? 'Сохраняем…' : current ? 'Сохранить' : 'Создать'}</button></div>
      </form>
    </div>
    <div role="tabpanel" id={`${id}-members-panel`} aria-labelledby={`${id}-members`} hidden={tab !== 'members'}>
      {current && tab === 'members' && <TeamMembers team={current} users={users} onPending={setMemberPending}/>}
    </div>
  </TeamDialog>;
}

function TeamMembers({ team, users, onPending }: { team: Team; users: readonly User[]; onPending: (pending: boolean) => void }) {
  const [members, setMembers] = useState<TeamMember[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState('');
  const [error, setError] = useState('');
  const [pending, setPending] = useState(false);
  const [query, setQuery] = useState('');
  const [status, setStatus] = useState('all');
  const [adding, setAdding] = useState(false);
  const [search, setSearch] = useState('');
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const seq = useRef(0);
  const alive = useRef(false);
  const guard = useRef(false);
  const load = useCallback(async () => {
    const sequence = ++seq.current; setLoading(true); setLoadError('');
    try { const result = await client.teamMembers(team.id); if (seq.current === sequence) setMembers(result || []); }
    catch (cause) { if (seq.current === sequence) setLoadError(message(cause)); }
    finally { if (seq.current === sequence) setLoading(false); }
  }, [team.id]);
  useEffect(() => { alive.current = true; void load(); return () => { alive.current = false; seq.current++; }; }, [load]);
  const disabled = pending || loading || !!loadError || !!team.archived_at;
  const directory = new Map(users.map(user => [user.id, user]));
  const ids = new Set(members.map(member => member.id));
  const eligible = users.filter(user => !user.disabled && !ids.has(user.id));
  const candidates = eligible.filter(user => match(search, user.name, user.id));
  const chosen = eligible.filter(user => selected.has(user.id));
  const visible = members.filter(member => {
    const user = directory.get(member.id);
    return match(query, member.name, member.id) && (status === 'all' || !!user && (status === 'disabled') === user.disabled);
  });
  async function remove(member: TeamMember) {
    if (disabled || guard.current) return;
    guard.current = true; setPending(true); onPending(true); setError('');
    try { await client.removeTeamMember(team.id, member.id); await load(); }
    catch (cause) { if (alive.current) setError(message(cause)); }
    finally { guard.current = false; if (alive.current) setPending(false); onPending(false); }
  }
  async function add(event: FormEvent) {
    event.preventDefault(); if (disabled || guard.current || !chosen.length) return;
    guard.current = true; setPending(true); onPending(true); setError('');
    const failed: { id: string; text: string }[] = [];
    try {
      for (const user of chosen) {
        try { await client.setTeamMember(team.id, user.id); }
        catch (cause) { failed.push({ id: user.id, text: `${user.name || user.id} — ${message(cause)}` }); }
      }
      if (alive.current) {
        setSelected(new Set(failed.map(item => item.id))); setSearch('');
        if (failed.length) setError(`Добавлено: ${chosen.length - failed.length} из ${chosen.length}. Не удалось добавить: ${failed.map(item => item.text).join('; ')}. Повторная попытка добавит только оставшихся выбранных участников.`);
        await load();
        if (alive.current && !failed.length) setAdding(false);
      }
    } finally { guard.current = false; if (alive.current) setPending(false); onPending(false); }
  }
  return <div className="admin-team-members" aria-busy={pending || loading}>
    {team.archived_at && <p className="corp-admin-hint">Состав архивной команды сохранён. Для изменений восстановите команду.</p>}
    <div className="admin-team-member-filters">
      <label className="field"><span>Поиск участников</span><input type="search" placeholder="Имя или ID" value={query} onChange={event => setQuery(event.target.value)}/></label>
      <label className="field"><span>Статус пользователя</span><select value={status} onChange={event => setStatus(event.target.value)}><option value="all">Все пользователи</option><option value="active">Активные</option><option value="disabled">Отключённые</option></select></label>
      {!team.archived_at && <button className="button primary admin-access-add-button" type="button" title="Добавить участников" aria-label="Добавить участников" disabled={disabled} onClick={() => { setSelected(new Set()); setSearch(''); setError(''); setAdding(true); }}><Plus size={20}/></button>}
    </div>
    {loadError && <><p className="spaces-error" role="alert">{loadError}</p><button className="button ghost" disabled={pending || loading} onClick={() => void load()}>Повторить загрузку</button></>}
    {error && !adding && <p className="spaces-error" role="alert">{error}</p>}
    <p className="admin-access-count">Участники: {visible.length}{visible.length !== members.length ? ` из ${members.length}` : ''}</p>
    <div className="admin-work-table-wrap"><table className="admin-work-table admin-team-members-table" aria-label="Участники команды">
      <colgroup><col/><col className="admin-team-status-col"/><col className="admin-team-actions-col"/></colgroup>
      <thead><tr><th scope="col">Пользователь</th><th scope="col">Статус</th><th scope="col">Действия</th></tr></thead>
      <tbody>{visible.map(member => <tr key={member.id}><th scope="row"><strong title={member.name || member.id}>{member.name || member.id}</strong>{member.name !== member.id && <small className="corp-admin-id" title={member.id}>{member.id}</small>}</th>
        <td>{!directory.has(member.id) ? 'Неизвестен' : directory.get(member.id)?.disabled ? 'Отключён' : 'Активен'}</td><td>{!team.archived_at && <button className="icon-button" type="button" title="Убрать из команды" aria-label={`Убрать из команды: ${member.name || member.id}`} disabled={disabled} onClick={() => void remove(member)}><Trash2 size={17}/></button>}</td></tr>)}</tbody>
    </table></div>
    {!visible.length && <p className="corp-admin-empty" role="status">{loading ? 'Загрузка участников…' : members.length ? 'Участники по этим фильтрам не найдены.' : 'В команде пока нет участников.'}</p>}
    {adding && <TeamDialog title="Добавить участников" small busy={pending} onClose={() => setAdding(false)}>
      <form className="admin-space-access-form admin-team-add-form" onSubmit={add}>
        <p className="admin-access-description">Выберите пользователей, которых нужно добавить в команду. Выбор сохраняется при поиске.</p>
        <label className="field"><span>Найти пользователя</span><input data-initial-focus type="search" placeholder="Имя или ID" value={search} disabled={pending} onChange={event => setSearch(event.target.value)}/></label>
        <div className="admin-access-candidates" role="group" aria-label="Пользователи вне команды">{candidates.map(user => <label className="admin-access-candidate" key={user.id} data-selected={selected.has(user.id)}>
          <input type="checkbox" checked={selected.has(user.id)} disabled={pending || loading} onChange={event => { const checked = event.target.checked; setSelected(current => { const next = new Set(current); if (checked) next.add(user.id); else next.delete(user.id); return next; }); }}/>
          <span><strong>{user.name || user.id}</strong>{user.name !== user.id && <small title={user.id}>{user.id}</small>}</span>
        </label>)}{!candidates.length && <p className="corp-admin-empty">{search ? 'Пользователи не найдены.' : 'Все доступные пользователи уже в команде.'}</p>}</div>
        <p className="admin-access-selection-count" role="status">Выбрано пользователей: {chosen.length}</p>
        {error && <p className="spaces-error" role="alert">{error}</p>}
        {loadError && <><p className="spaces-error" role="alert">{loadError}</p><button className="button ghost" type="button" disabled={pending || loading} onClick={() => void load()}>Повторить загрузку</button></>}
        <div className="spaces-actions admin-access-add-actions"><button className="button ghost" type="button" disabled={pending} onClick={() => setAdding(false)}>Отмена</button><button className="button primary" type="submit" disabled={disabled || !chosen.length}>{pending ? 'Добавляем…' : `Добавить${chosen.length ? ` (${chosen.length})` : ''}`}</button></div>
      </form>
    </TeamDialog>}
  </div>;
}
