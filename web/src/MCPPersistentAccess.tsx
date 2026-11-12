import { useCallback, useEffect, useRef, useState } from 'react';
import { Copy, Pencil, RefreshCw, Trash2 } from 'lucide-react';
import { AgentProject, client, MCPCredential, MCPServiceAccount } from './api';
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

function ProjectPicker({ value, onChange, projects, disabled }: { value: string[]; onChange: (next: string[]) => void; projects: AgentProject[]; disabled: boolean }) {
  const toggle = (id: string) => onChange(value.includes(id) ? value.filter(item => item !== id) : [...value, id]);
  const known = new Set(projects.map(project => project.id));
  return <fieldset className="mcp-persistent-scope">
    <legend>Проекты ключа <span>Выбрано: {value.length} из {MAX_PROJECTS}</span></legend>
    {projects.length ? <div className="mcp-persistent-projects">{projects.map(project => <label className="mcp-persistent-project" key={project.id}>
      <input type="checkbox" checked={value.includes(project.id)} disabled={disabled || (!value.includes(project.id) && value.length >= MAX_PROJECTS)} onChange={() => toggle(project.id)}/>
      <span>{project.name} <span className="mcp-persistent-chip">{VISIBILITY[project.visibility]}</span></span>
    </label>)}</div> : <p className="mcp-persistent-empty">Проектов пока нет. Создайте проект на вкладке «Проекты».</p>}
    {value.filter(id => !known.has(id)).map(id => <p className="mcp-persistent-hint" key={id}>Проект {id} вам больше не доступен; снимите выбор или он будет убран при сохранении.</p>)}
    <p className="mcp-persistent-hint">Агент с этим ключом работает в одном проекте, который указан в адресе подключения. Права агента в пространствах проекта не больше ваших. Общее пространство агент читает всегда.</p>
  </fieldset>;
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
    if (!trimmedName || trimmedName.length > 80 || !projectIds.length || projectIds.length > MAX_PROJECTS || credentialsEnabled !== true) return;
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
      setNotice('Ключ создан. Сохраните секрет сейчас: повторно он не показывается.');
    } catch (reason) {
      if (mounted.current) setError(explainError(reason));
    } finally {
      if (mounted.current) setBusy(false);
    }
  };

  const [editing, setEditing] = useState<{ id: string; projects: string[]; readOnly: boolean } | null>(null);

  const startEdit = (credential: MCPCredential) => {
    setError('');
    setNotice('');
    setEditing({ id: credential.id, projects: (credential.projects || []).filter(project => project.available).map(project => project.id), readOnly: credential.read_only });
  };

  const saveEdit = async () => {
    if (!editing || !editing.projects.length || editing.projects.length > MAX_PROJECTS) return;
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
      if (mounted.current) setBusy(false);
    }
  };

  const revokeCredential = async (id: string) => {
    setSecret('');
    setError('');
    setNotice('');
    setBusy(true);
    try {
      await client.revokeMCPCredential(id);
      if (!mounted.current) return;
      setCredentials(current => current.map(item => item.id === id ? { ...item, revoked_at: new Date().toISOString() } : item));
      setNotice('Постоянный ключ отозван.');
    } catch (reason) {
      if (mounted.current) setError(explainError(reason));
    } finally {
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


  return <section className="mcp-persistent" aria-labelledby="mcp-persistent-title" aria-busy={busy}>
    <div className="mcp-persistent-heading">
      <div><h2 id="mcp-persistent-title">Постоянный MCP-доступ</h2><p>Создавайте собственные ключи с ограниченным сроком и списком проектов, в которых может работать агент.</p></div>
      <button type="button" className="mcp-persistent-secondary" onClick={() => void load()} disabled={busy} aria-label="Обновить постоянные ключи"><RefreshCw size={15}/>Обновить</button>
    </div>

    <aside className="mcp-persistent-warning">
      Каждый запрос проверяется по текущим правам владельца: ключ не даёт больше, чем есть у вас сейчас. Агент с ключом работает в одном проекте из списка ключа; итоговое право в пространстве — меньшее из режима в проекте и ваших прав. Общее пространство ключ читает всегда. Выход из системы не отзывает ключ. Ключ перестаёт работать после истечения срока, отзыва, отключения владельца администратором или если владелец не входил в вики больше 14 дней (снова заработает после входа). Максимальный срок — 180 дней.
    </aside>

    {credentialsEnabled === false && <div className="mcp-persistent-unavailable" role="status">Постоянный MCP-доступ недоступен в этом режиме работы вики.</div>}
    {serviceAccountsEnabled === false && <div className="mcp-persistent-unavailable" role="status">Управление сервисными учётными записями недоступно в этом режиме работы вики.</div>}
    {error && <div className="mcp-persistent-error" role="alert">{error}</div>}
    {notice && <div className="mcp-persistent-status" role="status">{notice}</div>}

    {secret && <section className="mcp-persistent-secret" aria-labelledby="mcp-persistent-secret-title">
      <h3 id="mcp-persistent-secret-title">Секрет показан только сейчас</h3>
      <p>Скопируйте и сохраните его в настройках клиента MCP. Очистка секрета не отзывает созданный ключ.</p>
      {connectHint.length > 0 && <p>Подключение: <code>{window.location.origin}/mcp?project={connectHint[0]}</code> — агент сам предложит выбрать проект при первом подключении.</p>}
      <textarea aria-label="Секрет постоянного MCP-ключа" readOnly value={secret} rows={3} spellCheck={false} />
      <div className="mcp-persistent-actions">
        <button type="button" className="mcp-persistent-secondary" onClick={() => void copySecret()}><Copy size={15}/>Скопировать</button>
        <button type="button" className="mcp-persistent-secondary" onClick={() => { setSecret(''); setConnectHint([]); setNotice('Секрет очищен.'); }}>Очистить секрет</button>
      </div>
    </section>}

    {credentialsEnabled === true && <form className="mcp-persistent-form" onSubmit={event => void createCredential(event)}>
      <h3>Создать ключ</h3>
      <label className="mcp-persistent-label">Название ключа
        <input value={name} onChange={event => setName(event.target.value)} maxLength={80} required aria-describedby="mcp-persistent-name-limit" />
      </label>
      <small id="mcp-persistent-name-limit">До 80 символов</small>

      <ProjectPicker value={projectIds} onChange={setProjectIds} projects={agentProjects} disabled={busy}/>
      <label className="mcp-persistent-choice"><input type="checkbox" checked={readOnly} onChange={event => setReadOnly(event.target.checked)} disabled={busy}/>Только чтение</label>

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
      <button className="mcp-persistent-primary" type="submit" disabled={busy || credentialsEnabled !== true || !name.trim() || name.trim().length > 80 || projectIds.length < 1 || projectIds.length > MAX_PROJECTS}>
        Создать ключ
      </button>
    </form>}

    <section className="mcp-persistent-accounts" aria-labelledby="mcp-persistent-accounts-title">
      <h3 id="mcp-persistent-accounts-title">Мои сервисные учётные записи</h3>
      <p className="mcp-persistent-hint">Это ваши собственные учётные записи. Отключение навсегда отключает запись и отзывает все связанные ключи.</p>
      {serviceAccountsEnabled === true && <form className="mcp-persistent-account-create" onSubmit={event => void createServiceAccount(event)}>
        <label className="mcp-persistent-label">Название сервисной учётной записи
          <input value={serviceAccountName} onChange={event => setServiceAccountName(event.target.value)} maxLength={80} required />
        </label>
        <button className="mcp-persistent-secondary" type="submit" disabled={busy || !serviceAccountName.trim() || serviceAccountName.trim().length > 80}>Создать учётную запись</button>
      </form>}
      {serviceAccounts.length ? <div className="mcp-persistent-cards">{serviceAccounts.map(account => <article className="mcp-persistent-card" key={account.id}>
        <div className="mcp-persistent-card-content">{renaming?.id === account.id
          ? <form className="mcp-persistent-actions" onSubmit={event => { event.preventDefault(); void renameServiceAccount(); }}>
              <input aria-label="Новое название сервисной учётной записи" value={renaming.name} maxLength={80} required autoFocus onChange={event => setRenaming({ id: account.id, name: event.target.value })}/>
              <button type="submit" className="mcp-persistent-primary" disabled={busy || !renaming.name.trim() || renaming.name.trim().length > 80}>Сохранить</button>
              <button type="button" className="mcp-persistent-secondary" disabled={busy} onClick={() => setRenaming(null)}>Отмена</button>
              <small className="mcp-persistent-hint">Новое имя сразу появится в истории правок, в том числе в старых</small>
            </form>
          : <h4>{account.name}</h4>}<p><strong>ID:</strong> {account.id}</p><p><strong>Создана:</strong> <time dateTime={account.created_at}>{dateTime(account.created_at)}</time></p></div>
        {renaming?.id !== account.id && confirmDisableId !== account.id && <button type="button" className="mcp-persistent-secondary" onClick={() => { setSecret(''); setRenaming({ id: account.id, name: account.name }); }} disabled={busy}><Pencil size={15}/>Переименовать</button>}
        {confirmDisableId === account.id ? <div className="mcp-persistent-confirm" role="group" aria-label={`Подтверждение отключения: ${account.name}`}>
          <span>Отключить навсегда и отозвать все связанные ключи?</span>
          <button type="button" className="mcp-persistent-danger" onClick={() => void disableServiceAccount(account.id)} disabled={busy}>Отключить</button>
          <button type="button" className="mcp-persistent-secondary" onClick={() => setConfirmDisableId('')} disabled={busy}>Отмена</button>
        </div> : <button type="button" className="mcp-persistent-danger" onClick={() => { setSecret(''); setConfirmDisableId(account.id); }} disabled={busy}><Trash2 size={15}/>Отключить</button>}
      </article>)}</div> : <p className="mcp-persistent-empty">Сервисных учётных записей пока нет.</p>}
    </section>

    <section className="mcp-persistent-list" aria-labelledby="mcp-persistent-list-title">
      <h3 id="mcp-persistent-list-title">Мои ключи</h3>
      {credentials.length ? <div className="mcp-persistent-cards">{credentials.map(credential => {
        const expired = Date.parse(credential.expires_at) <= Date.now();
        const revoked = Boolean(credential.revoked_at);
        return <article className="mcp-persistent-card" key={credential.id}>
          <div className="mcp-persistent-card-content">
            <h4>{credential.name}</h4>
            <p><strong>Статус:</strong> {revoked ? 'Отозван' : expired ? 'Истёк' : 'Активен'}</p>
            <p><strong>От имени:</strong> {credential.service_account_id ? serviceAccounts.find(account => account.id === credential.service_account_id)?.name || credential.service_account_id : 'Моя учётная запись'}</p>
            <div className="mcp-persistent-chips"><strong>Проекты:</strong>{(credential.projects || []).map(project => <span className={`mcp-persistent-chip${project.available ? '' : ' is-unavailable'}`} key={project.id} title={project.available ? undefined : 'Проект удалён или недоступен'}>{project.name || project.id} · {VISIBILITY[project.visibility] || project.visibility}{project.available ? '' : ' · недоступен'}</span>)}{credential.read_only && <span className="mcp-persistent-chip">только чтение</span>}</div>
            <p><strong>Создан:</strong> <time dateTime={credential.created_at}>{dateTime(credential.created_at)}</time></p>
            <p><strong>Истекает:</strong> <time dateTime={credential.expires_at}>{dateTime(credential.expires_at)}</time></p>
          </div>
          {editing?.id === credential.id && <div className="mcp-persistent-form mcp-persistent-edit">
            <ProjectPicker value={editing.projects} onChange={next => setEditing({ ...editing, projects: next })} projects={agentProjects} disabled={busy}/>
            <label className="mcp-persistent-choice"><input type="checkbox" checked={editing.readOnly} onChange={event => setEditing({ ...editing, readOnly: event.target.checked })} disabled={busy}/>Только чтение</label>
            <div className="mcp-persistent-actions">
              <button type="button" className="mcp-persistent-primary" onClick={() => void saveEdit()} disabled={busy || editing.projects.length < 1 || editing.projects.length > MAX_PROJECTS}>Сохранить</button>
              <button type="button" className="mcp-persistent-secondary" onClick={() => setEditing(null)} disabled={busy}>Отмена</button>
            </div>
          </div>}
          {!revoked && !expired && editing?.id !== credential.id && <div className="mcp-persistent-actions">
            <button type="button" className="mcp-persistent-secondary" onClick={() => startEdit(credential)} disabled={busy}><Pencil size={15}/>Изменить</button>
            <button type="button" className="mcp-persistent-danger" onClick={() => void revokeCredential(credential.id)} disabled={busy}><Trash2 size={15}/>Отозвать</button>
          </div>}
        </article>;
      })}</div> : <p className="mcp-persistent-empty">Ключей пока нет.</p>}
    </section>
  </section>;
}
