import { useCallback, useEffect, useId, useRef, useState, type ReactNode } from 'react';
import { Copy, Info, Pencil, Plus, RefreshCw, Trash2, X } from 'lucide-react';
import { AgentProject, client, MCPCredential, MCPServiceAccount } from './api';
import './AdminSpaceDialogs.css';
import './MCPPersistentAccess.css';

const scopeErrors: Record<string, string> = {
  'use projects': 'Построчный доступ ключа убран: выберите проекты (Настройки - Проекты).',
  'space not visible': 'Одно из пространств проекта вам не видно.',
  'too many spaces': 'Слишком много строк в проекте: не больше 20.',
  'project is not available for this key': 'Проект недоступен для этого ключа: его нет в списке проектов ключа, он удалён или вам не виден.',
  'invalid project': 'Некорректный проект.',
};

const explainError = (error: unknown) => {
  const message = error instanceof Error ? error.message : 'Не удалось выполнить запрос.';
  if (scopeErrors[message]) return scopeErrors[message];
  return message === 'Недостаточно прав для этого действия.'
    ? 'Сервер отказал в доступе. Проверьте свои права и войдите снова.'
    : message;
};

const MAX_PROJECTS = 20;
const VISIBILITY: Record<string, string> = { private: 'личный', public: 'публичный' };

function KeyDialog({ title, busy, onClose, children, compact = false }: { title: string; busy: boolean; onClose: () => void; children: ReactNode; compact?: boolean }) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  useEffect(() => {
    const dialog = ref.current;
    const opener = document.activeElement;
    dialog?.showModal();
    dialog?.querySelector<HTMLElement>('[data-initial-focus]')?.focus();
    return () => { dialog?.close(); if (opener instanceof HTMLElement && opener.isConnected) opener.focus({ preventScroll: true }); };
  }, []);
  return <dialog ref={ref} className={`spaces-dialog admin-space-dialog mcp-key-dialog${compact ? ' mcp-key-dialog--compact' : ''}`} aria-labelledby={titleId}
    onCancel={event => { event.preventDefault(); event.stopPropagation(); if (!busy) onClose(); }}>
    <header className="spaces-head"><h2 id={titleId}>{title}</h2><button className="icon-button" type="button" aria-label={`Закрыть: ${title}`} disabled={busy} onClick={onClose}><X size={20}/></button></header>
    {children}
  </dialog>;
}

function ProjectPicker({ value, onChange, projects, disabled }: { value: string[]; onChange: (next: string[]) => void; projects: AgentProject[]; disabled: boolean }) {
  const [query, setQuery] = useState('');
  const toggle = (id: string) => onChange(value.includes(id) ? value.filter(item => item !== id) : [...value, id]);
  const known = new Set(projects.map(project => project.id));
  const needle = query.trim().toLocaleLowerCase();
  const visible = projects.filter(project => [project.name, project.description, project.owner_name].some(text => text?.toLocaleLowerCase().includes(needle)));
  return <fieldset className="mcp-persistent-scope">
    <legend>Проекты ключа <span>Выбрано: {value.length} из {MAX_PROJECTS}</span></legend>
    <label className="mcp-persistent-label">Поиск проектов<input type="search" placeholder="Название, описание или владелец" value={query} disabled={disabled} onChange={event => setQuery(event.target.value)}/></label>
    <div className="mcp-persistent-projects" role="group" aria-label="Проекты для ключа">{visible.map(project => <label className="mcp-persistent-project" key={project.id} data-selected={value.includes(project.id)}>
      <input type="checkbox" checked={value.includes(project.id)} disabled={disabled || (!value.includes(project.id) && value.length >= MAX_PROJECTS)} onChange={() => toggle(project.id)}/>
      <span><strong>{project.name}</strong> <span className="mcp-persistent-chip">{VISIBILITY[project.visibility]}</span>{project.description && <small>{project.description}</small>}</span>
    </label>)}{!visible.length && <p className="mcp-persistent-empty">{projects.length ? 'Проекты по этому запросу не найдены.' : 'Проектов пока нет. Создайте первый в разделе «Проекты».'}</p>}</div>
    {value.filter(id => !known.has(id)).map(id => <div className="mcp-key-unavailable-project" key={id}><span>Недоступный проект. Уберите его из выбора, чтобы сохранить изменения.</span><button className="button ghost" type="button" disabled={disabled} onClick={() => toggle(id)}>Убрать недоступный проект</button></div>)}
    <p className="mcp-persistent-hint">Выбор сохраняется при поиске.</p>
  </fieldset>;
}

function KeyScopeInfo() {
  return <div className="mcp-key-info"><Info size={20} aria-hidden="true"/><p>Агент с этим ключом работает в одном из выбранных проектов, указанном в адресе подключения. Его права в пространствах проекта не превышают ваши. Общее пространство агент читает всегда.</p></div>;
}

const dateTime = (value: string) => {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('ru-RU');
};

export default function MCPPersistentAccess() {
  const [agentProjects, setAgentProjects] = useState<AgentProject[]>([]);
  const [credentials, setCredentials] = useState<MCPCredential[]>([]);
  const [serviceAccounts, setServiceAccounts] = useState<MCPServiceAccount[]>([]);
  const [credentialsEnabled, setCredentialsEnabled] = useState<boolean | null>(null);
  const [serviceAccountsEnabled, setServiceAccountsEnabled] = useState<boolean | null>(null);
  const [keyQuery, setKeyQuery] = useState('');
  const [keyGroup, setKeyGroup] = useState<'active' | 'expired' | 'revoked'>('active');
  const [viewingProjects, setViewingProjects] = useState<MCPCredential | null>(null);
  const [creatingKey, setCreatingKey] = useState(false);
  const [revoking, setRevoking] = useState<MCPCredential | null>(null);
  const keyMutation = useRef(false);
  const [creatingAccount, setCreatingAccount] = useState(false);
  const [name, setName] = useState('');
  const [projectIds, setProjectIds] = useState<string[]>([]);
  const [readOnly, setReadOnly] = useState(false);
  const [connectHint, setConnectHint] = useState<string[]>([]);
  const [ttlDays, setTtlDays] = useState(30);
  const [serviceAccountId, setServiceAccountId] = useState('');
  const [serviceAccountName, setServiceAccountName] = useState('');
  const [confirmDisableId, setConfirmDisableId] = useState('');
  const [renaming, setRenaming] = useState<{ id: string; name: string } | null>(null);
  const [secret, setSecret] = useState('');
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState(false);
  const mounted = useRef(false);


  const load = useCallback(async () => {
    if (!mounted.current) return;
    setSecret('');
    setError('');
    setNotice('');
    setBusy(true);
    try {
      const [credentialResult, accountResult, projectList] = await Promise.all([
        client.mcpCredentials(),
        client.mcpServiceAccounts(),
        client.agentProjects(),
      ]);
      if (!mounted.current) return;
      setCredentials(credentialResult.credentials || []);
      setCredentialsEnabled(credentialResult.enabled);
      setServiceAccounts((accountResult.service_accounts || []).filter(account => !account.disabled));
      setServiceAccountsEnabled(accountResult.enabled);
      setAgentProjects(projectList);
    } catch (reason) {
      if (mounted.current) setError(explainError(reason));
    } finally {
      if (mounted.current) setBusy(false);
    }
  }, []);

  useEffect(() => {
    mounted.current = true;
    void load();
    return () => { mounted.current = false; };
  }, [load]);

  const createCredential = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const trimmedName = name.trim();
    if (keyMutation.current || busy || !trimmedName || trimmedName.length > 80 || !projectIds.length || projectIds.length > MAX_PROJECTS || projectIds.some(id => !agentProjects.some(project => project.id === id)) || credentialsEnabled !== true) return;
    keyMutation.current = true;
    setSecret('');
    setError('');
    setNotice('');
    setBusy(true);
    try {
      const result = await client.createMCPCredential({
        name: trimmedName,
        projects: projectIds,
        read_only: readOnly,
        ttl_seconds: ttlDays * 24 * 60 * 60,
        ...(serviceAccountId ? { service_account_id: serviceAccountId } : {}),
      });
      if (!mounted.current) return;
      const { token, ...credential } = result;
      setCredentials(current => [credential, ...current.filter(item => item.id !== credential.id)]);
      setSecret(token);
      setConnectHint(projectIds);
      setName('');
      setCreatingKey(false);
      setKeyQuery('');
      setKeyGroup('active');
      setNotice('Ключ создан. Сохраните секрет сейчас: повторно он не показывается.');
    } catch (reason) {
      if (mounted.current) setError(explainError(reason));
    } finally {
      keyMutation.current = false;
      if (mounted.current) setBusy(false);
    }
  };

  const [editing, setEditing] = useState<{ id: string; projects: string[]; readOnly: boolean } | null>(null);
  const editingId = editing?.id;



  const startEdit = (credential: MCPCredential) => {
    setError('');
    setNotice('');
    setCreatingKey(false);
    setEditing({ id: credential.id, projects: (credential.projects || []).map(project => project.id), readOnly: credential.read_only });
  };

  const saveEdit = async () => {
    if (keyMutation.current || busy || credentialsEnabled !== true || !editing || !editing.projects.length || editing.projects.length > MAX_PROJECTS || editing.projects.some(id => !agentProjects.some(project => project.id === id))) return;
    keyMutation.current = true;
    setSecret('');
    setError('');
    setNotice('');
    setBusy(true);
    try {
      const updated = await client.updateMCPCredential(editing.id, { projects: editing.projects, read_only: editing.readOnly });
      if (!mounted.current) return;
      setCredentials(current => current.map(item => item.id === updated.id ? { ...item, ...updated } : item));
      setEditing(null);
      setNotice('Ключ обновлён. Секрет прежний — в клиенте ничего менять не нужно.');
    } catch (reason) {
      if (mounted.current) setError(explainError(reason));
    } finally {
      keyMutation.current = false;
      if (mounted.current) setBusy(false);
    }
  };

  const revokeCredential = async (id: string) => {
    if (keyMutation.current || busy) return;
    keyMutation.current = true;
    setSecret('');
    setError('');
    setNotice('');
    setBusy(true);
    try {
      await client.revokeMCPCredential(id);
      if (!mounted.current) return;
      setCredentials(current => current.map(item => item.id === id ? { ...item, revoked_at: new Date().toISOString() } : item));
      setRevoking(null);
      setNotice('Постоянный ключ отозван.');
    } catch (reason) {
      if (mounted.current) setError(explainError(reason));
    } finally {
      keyMutation.current = false;
      if (mounted.current) setBusy(false);
    }
  };

  const createServiceAccount = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const trimmedName = serviceAccountName.trim();
    if (!trimmedName || trimmedName.length > 80 || serviceAccountsEnabled !== true) return;
    setSecret('');
    setError('');
    setNotice('');
    setBusy(true);
    try {
      const account = await client.createMCPServiceAccount(trimmedName);
      if (!mounted.current) return;
      setServiceAccounts(current => [account, ...current.filter(item => item.id !== account.id)]);
      setServiceAccountName('');
      setServiceAccountId(account.id);
      setCreatingAccount(false);
      setNotice('Сервисная учётная запись создана.');
    } catch (reason) {
      if (mounted.current) setError(explainError(reason));
    } finally {
      if (mounted.current) setBusy(false);
    }
  };

  const renameServiceAccount = async () => {
    if (!renaming) return;
    const trimmed = renaming.name.trim();
    if (!trimmed || trimmed.length > 80) return;
    setSecret('');
    setError('');
    setNotice('');
    setBusy(true);
    try {
      const updated = await client.renameMCPServiceAccount(renaming.id, trimmed);
      if (!mounted.current) return;
      setServiceAccounts(current => current.map(account => account.id === updated.id ? { ...account, ...updated } : account));
      setRenaming(null);
      setNotice('Сервисная учётная запись переименована.');
    } catch (reason) {
      if (mounted.current) setError(explainError(reason));
    } finally {
      if (mounted.current) setBusy(false);
    }
  };

  const disableServiceAccount = async (id: string) => {
    setSecret('');
    setError('');
    setNotice('');
    setBusy(true);
    try {
      await client.disableMCPServiceAccount(id);
      if (!mounted.current) return;
      setServiceAccounts(current => current.filter(account => account.id !== id));
      setCredentials(current => current.map(credential => credential.service_account_id === id && !credential.revoked_at
        ? { ...credential, revoked_at: new Date().toISOString() }
        : credential));
      if (serviceAccountId === id) setServiceAccountId('');
      setConfirmDisableId('');
      setNotice('Сервисная учётная запись отключена; связанные ключи отозваны.');
    } catch (reason) {
      if (mounted.current) setError(explainError(reason));
    } finally {
      if (mounted.current) setBusy(false);
    }
  };

  const copySecret = async () => {
    try {
      await navigator.clipboard.writeText(secret);
      if (mounted.current) setNotice('Секрет скопирован в буфер обмена.');
    } catch {
      if (mounted.current) setError('Не удалось скопировать секрет. Вы можете выделить его и скопировать вручную.');
    }
  };

  const query = keyQuery.trim().toLocaleLowerCase('ru-RU');
  const now = Date.now();
  const credentialStatus = (credential: MCPCredential) => credential.revoked_at ? 'revoked' : Date.parse(credential.expires_at) <= now ? 'expired' : 'active';
  const keyGroups = [
    { id: 'active' as const, label: 'Активные', empty: 'Активных ключей пока нет.' },
    { id: 'expired' as const, label: 'Истёкшие', empty: 'Ключей с истёкшим сроком нет.' },
    { id: 'revoked' as const, label: 'Отозванные', empty: 'Отозванных ключей пока нет.' },
  ];
  const visibleCredentials = credentials.filter(credential => credentialStatus(credential) === keyGroup && credential.name.toLocaleLowerCase('ru-RU').includes(query));
  const editedCredential = credentials.find(credential => credential.id === editingId);
  const disablingAccount = serviceAccounts.find(account => account.id === confirmDisableId);

  return <div className="mcp-persistent" aria-busy={busy}>
    {error && !creatingKey && !editing && !revoking && !secret && <div className="mcp-persistent-error" role="alert">{error}</div>}
    {notice && !secret && <div className="mcp-persistent-status" role="status">{notice}</div>}

    <section className="admin-work-panel" aria-labelledby="mcp-persistent-list-title">
      <div className="admin-work-toolbar mcp-persistent-toolbar">
        <h3 id="mcp-persistent-list-title">Мои ключи</h3>
        <div className="mcp-persistent-toolbar-actions">
          {credentials.length > 0 && <input type="search" aria-label="Поиск по названию ключа" placeholder="Поиск по названию" value={keyQuery} onChange={event => setKeyQuery(event.target.value)}/>}
          {credentialsEnabled === true && <button type="button" className="mcp-persistent-primary" aria-haspopup="dialog" onClick={() => { setError(''); setName(''); setProjectIds([]); setReadOnly(false); setTtlDays(30); setServiceAccountId(''); setCreatingKey(true); setEditing(null); }} disabled={busy}><Plus size={15}/>Создать ключ</button>}
          <button type="button" className="mcp-persistent-secondary" onClick={() => void load()} disabled={busy}><RefreshCw size={15}/>Обновить</button>
        </div>
      </div>

      <div className="admin-space-catalog-tabs mcp-key-tabs" role="group" aria-label="Статус ключей">
        {keyGroups.map(group => <button key={group.id} type="button" className="button ghost" aria-pressed={keyGroup === group.id} onClick={() => setKeyGroup(group.id)}>{group.label} <span>{credentials.filter(credential => credentialStatus(credential) === group.id).length}</span></button>)}
      </div>

      {credentialsEnabled === false && <div className="mcp-persistent-unavailable" role="status">Постоянный MCP-доступ недоступен в этом режиме работы вики.</div>}

      {secret && <KeyDialog title="Ключ создан" compact busy={false} onClose={() => { setSecret(''); setConnectHint([]); }}><section className="mcp-persistent-secret" aria-labelledby="mcp-persistent-secret-title">
        <h4 id="mcp-persistent-secret-title">Секрет показан только сейчас</h4>
        {notice && <p role="status">{notice}</p>}
        <p>Скопируйте и сохраните его в настройках клиента MCP. Очистка секрета не отзывает созданный ключ.</p>
        {connectHint.length > 0 && <p>Адрес подключения <code>{window.location.origin}/mcp?project={connectHint[0]}</code>. Агент сам предложит выбрать проект при первом подключении.</p>}
        <textarea aria-label="Секрет постоянного MCP-ключа" readOnly value={secret} rows={3} spellCheck={false} />
        {error && <p className="mcp-persistent-error" role="alert">{error}</p>}
        <div className="mcp-persistent-actions">
          <button type="button" className="mcp-persistent-secondary" onClick={() => void copySecret()}><Copy size={15}/>Скопировать</button>
          <button type="button" className="mcp-persistent-secondary" onClick={() => { setSecret(''); setConnectHint([]); setNotice('Секрет очищен.'); }}>Очистить секрет</button>
        </div>
      </section></KeyDialog>}

      {credentialsEnabled === true && creatingKey && <KeyDialog title="Создать ключ" busy={busy} onClose={() => setCreatingKey(false)}><form className="mcp-persistent-form" onSubmit={event => void createCredential(event)}>

        <label className="mcp-persistent-label">Название ключа
          <input value={name} onChange={event => setName(event.target.value)} maxLength={80} required disabled={busy} data-initial-focus/>
        </label>
        <ProjectPicker value={projectIds} onChange={setProjectIds} projects={agentProjects} disabled={busy}/>
        <div className="mcp-persistent-form-fields">
          <label className="mcp-persistent-label">Срок действия
            <select value={ttlDays} onChange={event => setTtlDays(Number(event.target.value))} disabled={busy}>
              <option value={1}>1 день</option><option value={7}>7 дней</option><option value={30}>30 дней</option><option value={90}>90 дней</option><option value={180}>180 дней</option>
            </select>
          </label>
          <label className="mcp-persistent-label">От чьего имени действует ключ
            <select value={serviceAccountId} onChange={event => setServiceAccountId(event.target.value)} disabled={busy || serviceAccountsEnabled === false}>
              <option value="">Моя учётная запись</option>
              {serviceAccounts.map(account => <option key={account.id} value={account.id}>{account.name}</option>)}
            </select>
          </label>
        </div>
        <label className="mcp-persistent-choice"><input type="checkbox" checked={readOnly} onChange={event => setReadOnly(event.target.checked)} disabled={busy}/>Только чтение</label>
        <KeyScopeInfo/>
        {error && <p className="mcp-persistent-error" role="alert">{error}</p>}
        <div className="mcp-persistent-actions mcp-key-footer">
          <button className="mcp-persistent-primary" type="submit" disabled={busy || !name.trim() || name.trim().length > 80 || projectIds.length < 1 || projectIds.length > MAX_PROJECTS}>Создать</button>
          <button className="mcp-persistent-secondary" type="button" onClick={() => setCreatingKey(false)} disabled={busy}>Отмена</button>
        </div>
      </form></KeyDialog>}

      {editing && editedCredential && <KeyDialog title={`Настройки ключа «${editedCredential.name}»`} busy={busy} onClose={() => setEditing(null)}><form className="mcp-persistent-form" onSubmit={event => { event.preventDefault(); void saveEdit(); }}>
        <p className="mcp-persistent-hint">Название, срок действия и учётная запись заданы при создании. Здесь можно изменить проекты и режим чтения; секрет останется прежним.</p>
        <ProjectPicker value={editing.projects} onChange={projects => setEditing({ ...editing, projects })} projects={agentProjects} disabled={busy}/>
        <label className="mcp-persistent-choice"><input type="checkbox" checked={editing.readOnly} onChange={event => setEditing({ ...editing, readOnly: event.target.checked })} disabled={busy}/>Только чтение</label>
        <KeyScopeInfo/>
        {error && <p className="mcp-persistent-error" role="alert">{error}</p>}
        <div className="mcp-persistent-actions mcp-key-footer">
          <button type="submit" className="mcp-persistent-primary" disabled={busy || credentialsEnabled !== true || editing.projects.length < 1 || editing.projects.length > MAX_PROJECTS || editing.projects.some(id => !agentProjects.some(project => project.id === id))}>Сохранить</button>
          <button type="button" className="mcp-persistent-secondary" onClick={() => setEditing(null)} disabled={busy}>Отмена</button>
        </div>
      </form></KeyDialog>}

      {revoking && <KeyDialog title={`Отозвать ключ «${revoking.name}»?`} compact busy={busy} onClose={() => setRevoking(null)}>
        <div className="mcp-persistent-form"><p className="mcp-key-revoke-warning">Ключ перестанет работать. Отменить отзыв нельзя; для повторного подключения потребуется создать новый ключ.</p>
          {error && <p className="mcp-persistent-error" role="alert">{error}</p>}
          <div className="mcp-persistent-actions mcp-key-footer"><button className="mcp-persistent-secondary" type="button" data-initial-focus disabled={busy} onClick={() => setRevoking(null)}>Нет</button><button className="mcp-persistent-danger" type="button" disabled={busy} onClick={() => void revokeCredential(revoking.id)}>{busy ? 'Отзываем…' : 'Да, отозвать'}</button></div>
        </div>
      </KeyDialog>}

      {viewingProjects && <KeyDialog title={`Проекты ключа «${viewingProjects.name}»`} busy={false} onClose={() => setViewingProjects(null)}>
        <ul className="mcp-key-project-list">{(viewingProjects.projects || []).map(project => <li key={project.id}>
          <div><strong>{project.name || project.id}</strong><span className="mcp-persistent-chip">{VISIBILITY[project.visibility] || project.visibility || 'Тип не указан'}</span></div>
          <small>{project.available ? 'Доступен' : 'Недоступен'}</small>
        </li>)}</ul>
        <div className="mcp-persistent-actions mcp-key-footer"><button type="button" className="mcp-persistent-secondary" data-initial-focus onClick={() => setViewingProjects(null)}>Закрыть</button></div>
      </KeyDialog>}

      {(credentialsEnabled === true || credentials.length > 0) && <div className="admin-work-table-wrap" role="region" aria-labelledby="mcp-persistent-list-title" tabIndex={0}>
        <table className="admin-work-table mcp-persistent-key-table">
          <thead>
            <tr>
              <th scope="col">Название</th>
              <th scope="col">Статус</th>
              <th scope="col">От имени</th>
              <th scope="col">Проекты</th>
              <th scope="col">Действует до</th>
              <th scope="col">Действия</th>
            </tr>
          </thead>
          <tbody>
            {visibleCredentials.map(credential => {
              const expired = Date.parse(credential.expires_at) <= now;
              const revoked = Boolean(credential.revoked_at);
              const projects = credential.projects || [];
              const accountId = credential.service_account_id;
              const owner = accountId ? serviceAccounts.find(account => account.id === accountId)?.name || accountId : 'Моя учётная запись';
              return <tr key={credential.id}>
                <th scope="row">
                  {credential.name}
                  {credential.read_only && <small className="mcp-persistent-meta">Только чтение</small>}
                </th>
                <td><span className={'mcp-persistent-state' + (revoked || expired ? '' : ' is-active')}>{revoked ? 'Отозван' : expired ? 'Истёк' : 'Активен'}</span></td>
                <td>{owner}</td>
                <td>
                  {projects.length > 0 ? <button type="button" className="mcp-key-project-button" aria-haspopup="dialog" aria-label={`Проекты ключа «${credential.name}»: ${projects.length}`} onClick={() => setViewingProjects(credential)}>Проектов: {projects.length}</button> : '—'}
                </td>
                <td>
                  <time dateTime={credential.expires_at}>{dateTime(credential.expires_at)}</time>
                  <small className="mcp-persistent-meta">Создан <time dateTime={credential.created_at}>{dateTime(credential.created_at)}</time></small>
                </td>
                <td>{!revoked && !expired ? <div className="mcp-persistent-actions">
                  <button type="button" className="icon-button" title="Редактировать ключ" aria-label={`Редактировать ключ «${credential.name}»`} aria-haspopup="dialog" onClick={() => startEdit(credential)} disabled={busy || credentialsEnabled !== true}><Pencil size={17}/></button>
                  <button type="button" className="icon-button" title="Отозвать ключ" aria-label={`Отозвать ключ «${credential.name}»`} aria-haspopup="dialog" onClick={() => { setError(''); setRevoking(credential); }} disabled={busy}><Trash2 size={17}/></button>
                </div> : '—'}</td>
              </tr>;
            })}
            {!visibleCredentials.length && <tr><td colSpan={6} className="mcp-persistent-empty">{query ? 'В этой группе ключи с таким названием не найдены.' : keyGroups.find(group => group.id === keyGroup)?.empty}</td></tr>}
          </tbody>
        </table>
      </div>}

      <details className="mcp-persistent-warning">
        <summary>Как работают права и срок действия ключа</summary>
        <p>Каждый запрос проверяется по текущим правам владельца. Агент работает в одном проекте из списка ключа. Право в пространстве определяется меньшим из режима проекта и ваших прав. Общее пространство ключ читает всегда.</p>
        <p>Выход из системы не отзывает ключ. Ключ перестаёт работать после истечения срока, отзыва или отключения владельца администратором. После 14 дней без входа ключ приостанавливается до следующего входа владельца. Максимальный срок — 180 дней.</p>
      </details>
    </section>

    <section className="admin-work-panel" aria-labelledby="mcp-persistent-accounts-title">
      <div className="admin-work-toolbar mcp-persistent-toolbar">
        <h3 id="mcp-persistent-accounts-title">Мои сервисные учётные записи</h3>
        {serviceAccountsEnabled === true && <button type="button" className="mcp-persistent-primary" aria-expanded={creatingAccount} onClick={() => { setCreatingAccount(!creatingAccount); setRenaming(null); setConfirmDisableId(''); }} disabled={busy}><Plus size={15}/>Создать учётную запись</button>}
      </div>
      {serviceAccountsEnabled === false && <div className="mcp-persistent-unavailable" role="status">Управление сервисными учётными записями недоступно в этом режиме работы вики.</div>}
      {serviceAccountsEnabled === true && creatingAccount && <form className="admin-work-detail mcp-persistent-form" onSubmit={event => void createServiceAccount(event)}>
        <label className="mcp-persistent-label">Название сервисной учётной записи
          <input value={serviceAccountName} onChange={event => setServiceAccountName(event.target.value)} maxLength={80} required disabled={busy} autoFocus />
        </label>
        <div className="mcp-persistent-actions">
          <button className="mcp-persistent-primary" type="submit" disabled={busy || !serviceAccountName.trim() || serviceAccountName.trim().length > 80}>Создать</button>
          <button className="mcp-persistent-secondary" type="button" onClick={() => setCreatingAccount(false)} disabled={busy}>Отмена</button>
        </div>
      </form>}
      {renaming && <form className="admin-work-detail mcp-persistent-form" onSubmit={event => { event.preventDefault(); void renameServiceAccount(); }}>
        <label className="mcp-persistent-label">Новое название сервисной учётной записи
          <input key={renaming.id} value={renaming.name} maxLength={80} required disabled={busy} autoFocus onChange={event => setRenaming({ ...renaming, name: event.target.value })}/>
        </label>
        <p className="mcp-persistent-hint">Новое имя сразу появится в истории правок, в том числе в старых.</p>
        <div className="mcp-persistent-actions">
          <button type="submit" className="mcp-persistent-primary" disabled={busy || !renaming.name.trim() || renaming.name.trim().length > 80}>Сохранить</button>
          <button type="button" className="mcp-persistent-secondary" disabled={busy} onClick={() => setRenaming(null)}>Отмена</button>
        </div>
      </form>}
      {disablingAccount && <div key={disablingAccount.id} className="admin-work-detail mcp-persistent-form" role="group" aria-label={'Подтверждение отключения: ' + disablingAccount.name}>
        <p>Отключить «{disablingAccount.name}» навсегда и отозвать все связанные ключи?</p>
        <div className="mcp-persistent-actions">
          <button type="button" className="mcp-persistent-danger" onClick={() => void disableServiceAccount(disablingAccount.id)} disabled={busy}>Отключить</button>
          <button type="button" className="mcp-persistent-secondary" onClick={() => setConfirmDisableId('')} disabled={busy} autoFocus>Отмена</button>
        </div>
      </div>}
      {(serviceAccountsEnabled === true || serviceAccounts.length > 0) && <div className="admin-work-table-wrap" role="region" aria-labelledby="mcp-persistent-accounts-title" tabIndex={0}>
        <table className="admin-work-table mcp-persistent-account-table">
          <thead>
            <tr>
              <th scope="col">Название</th>
              <th scope="col">ID</th>
              <th scope="col">Дата создания</th>
              <th scope="col">Действия</th>
            </tr>
          </thead>
          <tbody>
            {serviceAccounts.map(account => <tr key={account.id}>
              <th scope="row">{account.name}</th>
              <td><code>{account.id}</code></td>
              <td><time dateTime={account.created_at}>{dateTime(account.created_at)}</time></td>
              <td><div className="mcp-persistent-actions">
                <button type="button" className="mcp-persistent-secondary" aria-expanded={renaming?.id === account.id} onClick={() => { setSecret(''); setCreatingAccount(false); setConfirmDisableId(''); setRenaming(renaming?.id === account.id ? null : { id: account.id, name: account.name }); }} disabled={busy}><Pencil size={14}/>Переименовать</button>
                <button type="button" className="mcp-persistent-danger" aria-expanded={confirmDisableId === account.id} onClick={() => { setSecret(''); setCreatingAccount(false); setRenaming(null); setConfirmDisableId(confirmDisableId === account.id ? '' : account.id); }} disabled={busy}><Trash2 size={14}/>Отключить</button>
              </div></td>
            </tr>)}
            {!serviceAccounts.length && <tr><td colSpan={4} className="mcp-persistent-empty">Сервисных учётных записей пока нет.</td></tr>}
          </tbody>
        </table>
      </div>}
      {(serviceAccountsEnabled === true || serviceAccounts.length > 0) && <p className="mcp-persistent-account-hint mcp-persistent-hint">Отключение учётной записи отзывает все её ключи.</p>}
    </section>
  </div>;
}
