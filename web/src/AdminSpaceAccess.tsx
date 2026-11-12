import { type FormEvent, type ReactNode, useCallback, useEffect, useId, useRef, useState } from 'react';
import { Plus, Search, X } from 'lucide-react';
import { type AccessGrant, client, type Team, type TeamMember } from './api';

type Role = AccessGrant['role'];
type SubjectType = AccessGrant['subject_type'];

const ROLE_LABELS: Record<Role, string> = {
  owner: 'Владелец', manager: 'Менеджер', editor: 'Редактор', commenter: 'Комментатор', reader: 'Читатель',
};
const SUBJECT_LABELS: Record<SubjectType, string> = { user: 'Пользователь', team: 'Команда' };
const ROLES = Object.keys(ROLE_LABELS) as Role[];
const NON_OWNER_ROLES = ROLES.filter(role => role !== 'owner');

const grantKey = (grant: AccessGrant) => `${grant.subject_type}:${grant.subject_id}`;
const message = (error: unknown) => error instanceof Error ? error.message : String(error);
const normalize = (value: string) => value.trim().toLocaleLowerCase();
const matches = (needle: string, id: string, name?: string) =>
  normalize(id).includes(needle) || !!name && normalize(name).includes(needle);

type Props = {
  spaceId: string;
  archived: boolean;
  canChangeOwner: boolean;
  teams: readonly Team[];
  onChanged: () => void;
  onPendingChange: (pending: boolean) => void;
};

export function AdminSpaceAccess(props: Props) {
  return <SpaceGrants key={props.spaceId} {...props}/>;
}

function SpaceGrants({ spaceId, archived, canChangeOwner, teams, onChanged, onPendingChange }: Props) {
  const [grants, setGrants] = useState<AccessGrant[]>([]);
  const [directory, setDirectory] = useState<TeamMember[]>([]);
  const [loading, setLoading] = useState(true);
  const [grantError, setGrantError] = useState('');
  const [directoryError, setDirectoryError] = useState('');
  const [mutationError, setMutationError] = useState('');
  const [pending, setPending] = useState(false);
  const [query, setQuery] = useState('');
  const [typeFilter, setTypeFilter] = useState<SubjectType | ''>('');
  const [roleFilter, setRoleFilter] = useState<Role | ''>('');
  const [subjectType, setSubjectType] = useState<SubjectType>('user');
  const [recipientQuery, setRecipientQuery] = useState('');
  const [targetId, setTargetId] = useState('');
  const [selectedIds, setSelectedIds] = useState<Set<string>>(() => new Set());
  const [role, setRole] = useState<Role>('reader');
  const [adding, setAdding] = useState(false);
  const loadSequence = useRef(0);
  const mutating = useRef(false);
  const mounted = useRef(false);

  const load = useCallback(async () => {
    const sequence = ++loadSequence.current;
    setLoading(true);
    setGrantError('');
    setDirectoryError('');
    const [grantResult, directoryResult] = await Promise.allSettled([
      client.access('space', spaceId), client.users('space', spaceId),
    ]);
    if (sequence !== loadSequence.current) return;
    if (grantResult.status === 'fulfilled') {
      setGrants(grantResult.value || []);
    } else {
      setGrants([]);
      setGrantError(`Список доступа: ${message(grantResult.reason)}`);
    }
    if (directoryResult.status === 'fulfilled') {
      setDirectory(directoryResult.value || []);
    } else {
      setDirectory([]);
      setDirectoryError(`Список пользователей: ${message(directoryResult.reason)}`);
    }
    setLoading(false);
  }, [spaceId]);

  useEffect(() => {
    mounted.current = true;
    void load();
    return () => {
      mounted.current = false;
      loadSequence.current++;
    };
  }, [load]);

  const mutate = async (action: () => Promise<unknown>, afterSuccess?: () => void) => {
    if (archived || loading || grantError || mutating.current) return;
    mutating.current = true;
    setPending(true);
    onPendingChange(true);
    setMutationError('');
    try {
      await action();
    } catch (error) {
      if (mounted.current) setMutationError(message(error));
      return;
    } finally {
      mutating.current = false;
      if (mounted.current) setPending(false);
      onPendingChange(false);
    }
    if (mounted.current) {
      afterSuccess?.();
      void load();
    }
    onChanged();
  };

  const userNames = new Map(directory.map(user => [user.id, user.name]));
  const teamNames = new Map(teams.map(team => [team.id, team.name]));
  const subjectName = (grant: AccessGrant) =>
    (grant.subject_type === 'user' ? userNames : teamNames).get(grant.subject_id);
  const needle = normalize(query);
  const visibleGrants = grants.filter(grant =>
    (!typeFilter || grant.subject_type === typeFilter) &&
    (!roleFilter || grant.role === roleFilter) &&
    matches(needle, grant.subject_id, subjectName(grant)));

  const assigned = new Set(grants.filter(grant => grant.subject_type === subjectType).map(grant => grant.subject_id));
  const activeTeams = teams.filter(team => !team.archived_at);
  const manualTeam = subjectType === 'team' && teams.length === 0;
  const recipientNeedle = normalize(recipientQuery);
  const eligibleRecipients = (subjectType === 'user' ? directory : activeTeams).filter(recipient => !assigned.has(recipient.id));
  const candidates = eligibleRecipients.filter(recipient => matches(recipientNeedle, recipient.id, recipient.name));
  const candidateId = targetId.trim();
  const selectedRecipients = manualTeam
    ? (candidateId && !assigned.has(candidateId) ? [{ id: candidateId, name: candidateId }] : [])
    : eligibleRecipients.filter(recipient => selectedIds.has(recipient.id));
  const canAdd = selectedRecipients.length > 0;
  const disabled = loading || pending || !!grantError;
  const assignableRoles = canChangeOwner ? ROLES : NON_OWNER_ROLES;
  const selectedRole = assignableRoles.includes(role) ? role : 'reader';

  const add = async (event: FormEvent) => {
    event.preventDefault();
    if (!canAdd || archived || disabled || mutating.current) return;
    mutating.current = true;
    setPending(true);
    onPendingChange(true);
    setMutationError('');
    const failed: { id: string; name: string; error: string }[] = [];
    let succeeded = 0;
    try {
      // Keep the whole batch locked, including the refresh, so retries never duplicate successful grants.
      for (const recipient of selectedRecipients) {
        try {
          await client.setAccess('space', spaceId, { subject_type: subjectType, subject_id: recipient.id, role: selectedRole });
          succeeded++;
        } catch (error) {
          failed.push({ id: recipient.id, name: recipient.name || recipient.id, error: message(error) });
        }
      }
      if (mounted.current) {
        setSelectedIds(new Set(failed.map(recipient => recipient.id)));
        if (failed.length) {
          setRecipientQuery('');
          setMutationError(`Добавлено: ${succeeded} из ${selectedRecipients.length}. Не удалось выдать доступ: ${failed.map(recipient => `${recipient.name} — ${recipient.error}`).join('; ')}. Эти получатели остаются выбранными для повторной попытки.`);
        } else {
          setTargetId('');
          setRecipientQuery('');
        }
        await load();
        if (mounted.current && !failed.length) setAdding(false);
      }
      if (succeeded) onChanged();
    } finally {
      mutating.current = false;
      if (mounted.current) setPending(false);
      onPendingChange(false);
    }
  };

  return <div className="admin-space-access" aria-busy={loading || pending}>
    {archived && <p className="corp-admin-empty">Доступ к архивному пространству можно изменить после восстановления.</p>}
    {grantError && <p className="corp-admin-inline-error" role="alert">{grantError}</p>}
    {directoryError && <p className="corp-admin-inline-error" role="alert">{directoryError}</p>}
    {mutationError && !adding && <p className="corp-admin-inline-error" role="alert">{mutationError}</p>}
    {(grantError || directoryError) && <button className="button ghost" type="button" disabled={loading || pending}
      onClick={() => void load()}>Повторить загрузку</button>}

    {adding && !archived && <AddAccessDialog busy={pending} onClose={() => setAdding(false)}>
      <form className="admin-space-access-form" aria-label="Выдача доступа" onSubmit={add}>
        <p className="admin-access-description">Выберите одного или нескольких получателей. Одна роль будет назначена всем выбранным.</p>
        <label className="field"><span>Тип получателя</span><select aria-label="Тип нового получателя" value={subjectType} disabled={disabled} onChange={event => {
          setSubjectType(event.target.value as SubjectType);
          setTargetId('');
          setSelectedIds(new Set());
          setRecipientQuery('');
          setMutationError('');
        }}>
          <option value="user">Пользователь</option>
          <option value="team">Команда</option>
        </select></label>
        {manualTeam
          ? <label className="field"><span>ID команды</span><input value={targetId} onChange={event => setTargetId(event.target.value)} disabled={disabled}
            placeholder="ID команды" aria-label="ID команды"/></label>
          : <>
            <label className="field"><span>Найти {subjectType === 'user' ? 'пользователя' : 'команду'}</span><input data-recipient-search type="search" value={recipientQuery} disabled={disabled} onChange={event => {
              setRecipientQuery(event.target.value);
            }} placeholder="Введите имя или ID" aria-label="Поиск нового получателя"/></label>
            <div className="admin-access-candidates" role="group" aria-label="Получатели без доступа">
              {candidates.length === 0 && <p className="corp-admin-empty">{recipientQuery ? 'Ничего не найдено. Попробуйте другое имя.' : 'Нет доступных получателей без назначенной роли.'}</p>}
              {candidates.map(candidate => <label key={candidate.id} className="admin-access-candidate" data-selected={selectedIds.has(candidate.id)}>
                <input type="checkbox" name="recipient" value={candidate.id} checked={selectedIds.has(candidate.id)} disabled={disabled} onChange={event => {
                  const checked = event.target.checked;
                  setSelectedIds(current => {
                    const next = new Set(current);
                    if (checked) next.add(candidate.id); else next.delete(candidate.id);
                    return next;
                  });
                }}/>
                <span><strong title={candidate.name || candidate.id}>{candidate.name || candidate.id}</strong>
                  {candidate.name && candidate.name !== candidate.id && <small title={candidate.id}>{candidate.id}</small>}</span>
              </label>)}
            </div>
          </>}
        <p className="admin-access-selection-count" role="status">Выбрано получателей: {selectedRecipients.length}</p>
        <label className="field"><span>Роль для выбранных получателей</span><select aria-label="Новая роль" value={selectedRole} disabled={disabled} onChange={event => setRole(event.target.value as Role)}>
          {assignableRoles.map(value => <option key={value} value={value}>{ROLE_LABELS[value]}</option>)}
        </select></label>
        {directoryError && <p className="corp-admin-inline-error" role="alert">{directoryError}</p>}
        {grantError && <p className="corp-admin-inline-error" role="alert">{grantError}</p>}
        {(grantError || directoryError) && <button className="button ghost" type="button" disabled={loading || pending} onClick={() => void load()}>Повторить загрузку</button>}
        {mutationError && <p className="corp-admin-inline-error" role="alert">{mutationError}</p>}
        <div className="spaces-actions admin-access-add-actions">
          <button className="button ghost" type="button" disabled={pending} onClick={() => setAdding(false)}>Отмена</button>
          <button className="button primary" type="submit" disabled={disabled || !canAdd}>{pending ? 'Добавляем…' : `Добавить доступ${canAdd ? ` (${selectedRecipients.length})` : ''}`}</button>
        </div>
      </form>
    </AddAccessDialog>}

    <div className="admin-space-access-filters">
      <label className="field admin-access-search"><span>Поиск</span><div className="admin-access-search-input"><Search size={17}/><input type="search" value={query} onChange={event => setQuery(event.target.value)}
        placeholder="Имя или ID получателя" aria-label="Поиск получателей с доступом"/></div></label>
      <label className="field"><span>Тип получателя</span><select value={typeFilter} onChange={event => setTypeFilter(event.target.value as SubjectType | '')} aria-label="Фильтр по типу получателя">
        <option value="">Все получатели</option>
        <option value="user">Пользователи</option>
        <option value="team">Команды</option>
      </select></label>
      <label className="field"><span>Роль</span><select value={roleFilter} onChange={event => setRoleFilter(event.target.value as Role | '')} aria-label="Фильтр по роли">
        <option value="">Все роли</option>
        {ROLES.map(value => <option key={value} value={value}>{ROLE_LABELS[value]}</option>)}
      </select></label>
      {!archived && <button className="button primary admin-access-add-button" type="button" aria-label="Добавить доступ" title="Добавить доступ" disabled={disabled}
        onClick={() => { setMutationError(''); setTargetId(''); setSelectedIds(new Set()); setRecipientQuery(''); setRole('reader'); setAdding(true); }}><Plus size={20}/></button>}
    </div>
    {!loading && !grantError && <p className="admin-access-count">Получатели с доступом: {visibleGrants.length}{visibleGrants.length !== grants.length ? ` из ${grants.length}` : ''}</p>}

    {loading && <p className="corp-admin-empty" role="status">Загрузка списка доступа…</p>}
    {!loading && !grantError && visibleGrants.length === 0 &&
      <p className="corp-admin-empty" role="status">{grants.length ? 'Получатели по этим фильтрам не найдены.' : 'Явных выдач доступа пока нет.'}</p>}
    {visibleGrants.length > 0 && <div className="admin-work-table-wrap">
      <table className="admin-work-table admin-space-grants-table" aria-label="Выданный доступ">
        <colgroup><col/><col className="admin-access-type-col"/><col className="admin-access-role-col"/>{!archived && <col className="admin-access-actions-col"/>}</colgroup>
        <thead><tr>
          <th scope="col">Получатель</th>
          <th scope="col">Тип</th>
          <th scope="col">Роль</th>
          {!archived && <th scope="col">Действия</th>}
        </tr></thead>
        <tbody>{visibleGrants.map(grant => <GrantRow key={`${grantKey(grant)}:${grant.role}`}
          grant={grant} name={subjectName(grant)} archived={archived} disabled={disabled} assignableRoles={assignableRoles}
          onSave={role => void mutate(() => client.setAccess('space', spaceId, { ...grant, role }))}
          onRemove={() => void mutate(() => client.removeAccess('space', spaceId, grant))}/>)}</tbody>
      </table>
    </div>}

  </div>;
}

function GrantRow({ grant, name, archived, disabled, assignableRoles, onSave, onRemove }: {
  grant: AccessGrant;
  name?: string;
  archived: boolean;
  disabled: boolean;
  assignableRoles: readonly Role[];
  onSave: (role: Role) => void;
  onRemove: () => void;
}) {
  const [draft, setDraft] = useState(grant.role);
  const editable = !archived && assignableRoles.includes(grant.role);
  const role = assignableRoles.includes(draft) ? draft : grant.role;
  const label = name || grant.subject_id;
  return <tr>
    <th scope="row" className="admin-space-access-subject">
      <strong title={label}>{label}</strong>
      {label !== grant.subject_id && <small className="corp-admin-id" title={grant.subject_id}>{grant.subject_id}</small>}
    </th>
    <td>{SUBJECT_LABELS[grant.subject_type]}</td>
    <td>{editable
      ? <select className="admin-space-access-role" aria-label={`Роль: ${label}`} value={role} disabled={disabled}
        onChange={event => setDraft(event.target.value as Role)}>
        {assignableRoles.map(value => <option key={value} value={value}>{ROLE_LABELS[value]}</option>)}
      </select>
      : <span className="corp-admin-badge">{ROLE_LABELS[grant.role]}</span>}
    </td>
    {!archived && <td>{editable && <div className="admin-work-table-actions">
      {role !== grant.role && <button className="button ghost" type="button" disabled={disabled}
        onClick={() => onSave(role)}>Сохранить</button>}
      <button className="button ghost" type="button" aria-label={`Убрать доступ: ${label}`} disabled={disabled}
        onClick={onRemove}>Убрать доступ</button>
    </div>}</td>}
  </tr>;
}

function AddAccessDialog({ busy, onClose, children }: { busy: boolean; onClose: () => void; children: ReactNode }) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  useEffect(() => {
    const dialog = ref.current;
    const opener = document.activeElement;
    dialog?.showModal();
    dialog?.querySelector<HTMLInputElement>('[data-recipient-search]')?.focus();
    return () => {
      dialog?.close();
      if (opener instanceof HTMLElement && opener.isConnected) opener.focus({ preventScroll: true });
    };
  }, []);
  return <dialog ref={ref} className="spaces-dialog admin-access-add-dialog" aria-labelledby={titleId}
    onCancel={event => { event.preventDefault(); event.stopPropagation(); if (!busy) onClose(); }}>
    <header className="spaces-head"><h2 id={titleId}>Добавить доступ</h2>
      <button className="icon-button" type="button" aria-label="Закрыть добавление доступа" disabled={busy} onClick={onClose}><X size={20}/></button></header>
    {children}
  </dialog>;
}
