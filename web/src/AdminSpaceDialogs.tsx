import { useEffect, useId, useRef, useState, type FormEvent, type KeyboardEvent, type ReactNode } from 'react';
import { Archive, X } from 'lucide-react';
import { client, type Space, type Team } from './api';
import { AdminSpaceAccess } from './AdminSpaceAccess';
import './AdminSpaceDialogs.css';

type DialogProps = {
  title: string;
  compact?: boolean;
  busy: boolean;
  onClose: () => void;
  children: ReactNode;
};

function SpaceDialog({ title, compact = false, busy, onClose, children }: DialogProps) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();

  useEffect(() => {
    const dialog = ref.current;
    const opener = document.activeElement;
    dialog?.showModal();
    dialog?.querySelector<HTMLElement>('[data-initial-focus]')?.focus();
    return () => {
      dialog?.close();
      if (opener instanceof HTMLElement) opener.focus({ preventScroll: true });
    };
  }, []);

  return <dialog ref={ref} className={`spaces-dialog admin-space-dialog${compact ? ' admin-space-dialog--compact' : ''}`}
    aria-labelledby={titleId} onCancel={event => { event.preventDefault(); if (!busy) onClose(); }}>
    <header className="spaces-head">
      <h2 id={titleId}>{title}</h2>
      <button className="icon-button" type="button" aria-label="Закрыть" onClick={onClose} disabled={busy}><X size={19}/></button>
    </header>
    {children}
  </dialog>;
}

type SpaceActionProps = { space: Space; onChanged: () => void; onClose: () => void };
type EditTab = 'general' | 'access';

export function AdminSpaceEditDialog({ space, teams, isAdmin, personalMode, initialTab, onChanged, onClose }: Omit<SpaceActionProps, 'space'> & {
  space?: Space;
  teams: readonly Team[];
  isAdmin: boolean;
  personalMode: boolean;
  initialTab: EditTab;
}) {
  const [current, setCurrent] = useState(space);
  const permissions = space ?? current;
  const canEdit = !permissions || (!permissions.limited && (permissions.effective_role === 'manager' || permissions.effective_role === 'owner'));
  const canAccess = !!current && (canEdit || (isAdmin && Boolean(permissions?.limited)));
  const [tab, setTab] = useState<EditTab>(!space ? 'general' : canEdit ? initialTab : 'access');
  const [name, setName] = useState(space?.name ?? '');
  const [description, setDescription] = useState(space?.description ?? '');
  const [personal, setPersonal] = useState(true);
  const [original, setOriginal] = useState({ name: space?.name ?? '', description: space?.description ?? '' });
  const [saving, setSaving] = useState(false);
  const [changingAccess, setChangingAccess] = useState(false);
  const [error, setError] = useState('');
  const [saved, setSaved] = useState('');
  const savingRef = useRef(false);
  const id = useId();
  const busy = saving || changingAccess;
  const changed = name.trim() !== original.name || description !== original.description;

  function navigateTabs(event: KeyboardEvent<HTMLButtonElement>) {
    if (busy || !canEdit || !canAccess) return;
    if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
    event.preventDefault();
    const next = event.key === 'Home' ? 'general' : event.key === 'End' ? 'access' : tab === 'general' ? 'access' : 'general';
    setTab(next);
    document.getElementById(`${id}-tab-${next}`)?.focus();
  }

  async function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const nextName = name.trim();
    if (!canEdit || busy || savingRef.current || !nextName || !changed) return;
    savingRef.current = true;
    setSaving(true); setError(''); setSaved('');
    const creating = !current;
    try {
      const updated = current
        ? await client.updateSpace(current.id, {
          ...(nextName !== original.name && { name: nextName }),
          ...(description !== original.description && { description }),
        })
        : personalMode
          ? await client.createSpace(nextName, description)
          : await client.createSpaceFor({ name: nextName, description, personal: !isAdmin || personal });
      // Creation grants its authenticated actor ownership in the same server transaction.
      // The create response omits effective_role; PATCH can omit it too.
      setCurrent(current ? { ...current, ...updated } : { ...updated, effective_role: 'owner' });
      setName(updated.name);
      setDescription(updated.description ?? '');
      setOriginal({ name: updated.name, description: updated.description ?? '' });
      setSaved(creating ? 'Пространство создано' : 'Изменения сохранены');
      onChanged();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally { savingRef.current = false; setSaving(false); }
  }

  return <SpaceDialog title={current ? `Пространство «${original.name}»` : 'Создать пространство'} busy={busy} onClose={onClose}>
    <div className="admin-space-tabs" role="tablist" aria-label="Настройки пространства">
      {([['general', 'Общее', canEdit], ['access', 'Доступ', canAccess]] as const).map(([key, label, enabled]) =>
        <button key={key} className="button ghost" type="button" role="tab" id={`${id}-tab-${key}`}
          aria-controls={`${id}-panel-${key}`} aria-selected={tab === key}
          tabIndex={tab === key ? 0 : -1} disabled={!enabled || (busy && tab !== key)}
          data-initial-focus={current && tab === key ? '' : undefined}
          onClick={() => setTab(key)} onKeyDown={navigateTabs}>{label}</button>)}
    </div>
    <div className="admin-space-tab-panel" role="tabpanel" id={`${id}-panel-general`} aria-labelledby={`${id}-tab-general`} hidden={tab !== 'general'}>
      {canEdit && <form className="spaces-edit" onSubmit={save} aria-busy={saving}>
        <label className="field"><span>Название</span><input required maxLength={160} data-initial-focus={!current ? '' : undefined} value={name} disabled={saving}
          onChange={event => { setName(event.target.value); setSaved(''); }}/></label>
        <label className="field"><span>Описание</span><textarea rows={4} value={description} disabled={saving}
          onChange={event => { setDescription(event.target.value); setSaved(''); }}/></label>
        {!current && !personalMode && <label className="field"><span>Тип пространства</span>
          <select value={personal ? "personal" : "team"} disabled={saving} onChange={event => setPersonal(event.target.value === "personal")}>
            <option value="personal">Личное</option>
            {isAdmin && <option value="team">Командное</option>}
          </select>
        </label>}
        {error && <p className="spaces-error" role="alert">{error}</p>}
        <div className="spaces-actions">
          {saved && <span className="admin-space-saved" role="status">{saved}</span>}
          <button className="button primary" type="submit" disabled={saving || !changed || !name.trim()}>{saving ? (current ? 'Сохраняем…' : 'Создаём…') : current ? 'Сохранить' : 'Создать'}</button>
        </div>
      </form>}
    </div>
    <div className="admin-space-tab-panel" role="tabpanel" id={`${id}-panel-access`} aria-labelledby={`${id}-tab-access`} hidden={tab !== 'access'}>
      {tab === 'access' && current && canAccess && <AdminSpaceAccess spaceId={current.id} archived={Boolean(current.archived_at)}
        teams={teams} canChangeOwner={isAdmin || permissions?.effective_role === 'owner'}
        onChanged={onChanged} onPendingChange={setChangingAccess}/>}
    </div>
  </SpaceDialog>;
}

export function AdminSpaceArchiveDialog({ space, onChanged, onClose }: SpaceActionProps) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState('');

  async function archive(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (pending) return;
    setPending(true); setError('');
    try {
      await client.updateSpace(space.id, { archived: true });
      onChanged();
      onClose();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
      setPending(false);
    }
  }

  return <SpaceDialog title={`Архивировать «${space.name}»?`} compact busy={pending} onClose={onClose}>
    <form className="admin-space-confirm" onSubmit={archive} aria-busy={pending}>
      <p>Пространство исчезнет из списка активных. Его можно будет восстановить из архива.</p>
      {error && <p className="spaces-error" role="alert">{error}</p>}
      <div className="spaces-actions">
        <button className="button ghost" type="button" data-initial-focus onClick={onClose} disabled={pending}>Нет</button>
        <button className="button primary" type="submit" disabled={pending}><Archive size={15}/>{pending ? 'Архивируем…' : 'Да, архивировать'}</button>
      </div>
    </form>
  </SpaceDialog>;
}
