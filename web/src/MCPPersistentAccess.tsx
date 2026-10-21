import { useCallback, useEffect, useRef, useState } from 'react';
import { Copy, RefreshCw, Trash2 } from 'lucide-react';
import { client, MCPCredential, MCPServiceAccount, Project } from './api';
import './MCPPersistentAccess.css';

const explainError = (error: unknown) => {
  const message = error instanceof Error ? error.message : 'Не удалось выполнить запрос.';
  return message === 'Недостаточно прав для этого действия.'
    ? 'Сервер отказал в доступе. Проверьте свои права и войдите снова.'
    : message;
};

const dateTime = (value: string) => {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('ru-RU');
};

const canWrite = (project: Project) => ['editor', 'manager', 'owner'].includes(project.effective_role || '');

export default function MCPPersistentAccess() {
  const [projects, setProjects] = useState<Project[]>([]);
  const [credentials, setCredentials] = useState<MCPCredential[]>([]);
  const [serviceAccounts, setServiceAccounts] = useState<MCPServiceAccount[]>([]);
  const [credentialsEnabled, setCredentialsEnabled] = useState<boolean | null>(null);
  const [serviceAccountsEnabled, setServiceAccountsEnabled] = useState<boolean | null>(null);
  const [name, setName] = useState('');
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [mode, setMode] = useState<'read' | 'write'>('read');
  const [ttlDays, setTtlDays] = useState(7);
  const [serviceAccountId, setServiceAccountId] = useState('');
  const [serviceAccountName, setServiceAccountName] = useState('');
  const [confirmDisableId, setConfirmDisableId] = useState('');
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
      const [credentialResult, accountResult, catalogSpaces] = await Promise.all([
        client.mcpCredentials(),
        client.mcpServiceAccounts(),
        client.catalog(),
      ]);
      if (!mounted.current) return;
      const uniqueProjects = new Map<string, Project>();
      for (const space of catalogSpaces) {
        if (space.archived_at) continue;
        for (const project of space.projects || []) {
          if (!project.archived_at && !uniqueProjects.has(project.id)) uniqueProjects.set(project.id, project);
        }
      }
      const activeProjects = [...uniqueProjects.values()];
      setCredentials(credentialResult.credentials || []);
      setCredentialsEnabled(credentialResult.enabled);
      setServiceAccounts((accountResult.service_accounts || []).filter(account => !account.disabled));
      setServiceAccountsEnabled(accountResult.enabled);
      setProjects(activeProjects);
      const activeIds = new Set(activeProjects.map(project => project.id));
      setSelectedIds(current => current.filter(id => activeIds.has(id)));
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

  useEffect(() => {
    if (mode === 'write') setSelectedIds(current => current.filter(id => {
      const project = projects.find(item => item.id === id);
      return Boolean(project && canWrite(project));
    }));
  }, [mode, projects]);

  const toggleProject = (id: string, checked: boolean) => {
    setSelectedIds(current => {
      if (checked) return current.length < 20 && !current.includes(id) ? [...current, id] : current;
      return current.filter(item => item !== id);
    });
  };

  const projectCanWrite = (id: string) => {
    const project = projects.find(item => item.id === id);
    return Boolean(project && canWrite(project));
  };

  const createCredential = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const trimmedName = name.trim();
    if (!trimmedName || trimmedName.length > 80 || !selectedIds.length || selectedIds.length > 20 || credentialsEnabled !== true) return;
    if (mode === 'write' && selectedIds.some(id => !projectCanWrite(id))) return;
    setSecret('');
    setError('');
    setNotice('');
    setBusy(true);
    try {
      const result = await client.createMCPCredential({
        name: trimmedName,
        project_ids: selectedIds,
        mode,
        ttl_seconds: ttlDays * 24 * 60 * 60,
        ...(serviceAccountId ? { service_account_id: serviceAccountId } : {}),
      });
      if (!mounted.current) return;
      const { token, ...credential } = result;
      setCredentials(current => [credential, ...current.filter(item => item.id !== credential.id)]);
      setSecret(token);
      setName('');
      setNotice('Ключ создан. Сохраните секрет сейчас: повторно он не показывается.');
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

  const selectableProjects = projects.filter(project => mode === 'read' || canWrite(project));
  const projectNames = new Map(projects.map(project => [project.id, project.name]));

  return <section className="mcp-persistent" aria-labelledby="mcp-persistent-title" aria-busy={busy}>
    <div className="mcp-persistent-heading">
      <div><h2 id="mcp-persistent-title">Постоянный MCP-доступ</h2><p>Создавайте собственные ключи с ограниченным сроком и точным списком проектов.</p></div>
      <button type="button" className="mcp-persistent-secondary" onClick={() => void load()} disabled={busy} aria-label="Обновить постоянные ключи"><RefreshCw size={15}/>Обновить</button>
    </div>

    <aside className="mcp-persistent-warning">
      Каждый запрос проверяется по текущим правам владельца и правилам допуска Keycloak. Ключ действует только для выбранных проектов: дочерние проекты автоматически не включаются. Выход из системы не отзывает постоянный ключ; он прекращает действовать после истечения срока или отзыва. Максимальный срок — 30 дней.
    </aside>

    {credentialsEnabled === false && <div className="mcp-persistent-unavailable" role="status">Постоянный MCP-доступ отключён. Администратор должен настроить текущий механизм проверки допуска в Keycloak.</div>}
    {serviceAccountsEnabled === false && <div className="mcp-persistent-unavailable" role="status">Управление сервисными учётными записями отключено. Администратор должен настроить текущий механизм проверки допуска в Keycloak.</div>}
    {error && <div className="mcp-persistent-error" role="alert">{error}</div>}
    {notice && <div className="mcp-persistent-status" role="status">{notice}</div>}

    {secret && <section className="mcp-persistent-secret" aria-labelledby="mcp-persistent-secret-title">
      <h3 id="mcp-persistent-secret-title">Секрет показан только сейчас</h3>
      <p>Скопируйте и сохраните его в настройках клиента MCP. Очистка секрета не отзывает созданный ключ.</p>
      <textarea aria-label="Секрет постоянного MCP-ключа" readOnly value={secret} rows={3} spellCheck={false} />
      <div className="mcp-persistent-actions">
        <button type="button" className="mcp-persistent-secondary" onClick={() => void copySecret()}><Copy size={15}/>Скопировать</button>
        <button type="button" className="mcp-persistent-secondary" onClick={() => { setSecret(''); setNotice('Секрет очищен.'); }}>Очистить секрет</button>
      </div>
    </section>}

    {credentialsEnabled === true && <form className="mcp-persistent-form" onSubmit={event => void createCredential(event)}>
      <h3>Создать постоянный ключ</h3>
      <label className="mcp-persistent-label">Название ключа
        <input value={name} onChange={event => setName(event.target.value)} maxLength={80} required aria-describedby="mcp-persistent-name-limit" />
      </label>
      <small id="mcp-persistent-name-limit">До 80 символов</small>

      <fieldset>
        <legend>Режим доступа</legend>
        <label className="mcp-persistent-choice"><input type="radio" name="mcp-mode" value="read" checked={mode === 'read'} onChange={() => setMode('read')} disabled={busy}/>Только чтение</label>
        <label className="mcp-persistent-choice"><input type="radio" name="mcp-mode" value="write" checked={mode === 'write'} onChange={() => { setMode('write'); setSelectedIds(current => current.filter(projectCanWrite)); }} disabled={busy}/>Чтение и запись</label>
        {mode === 'write' && <p className="mcp-persistent-hint">Для записи доступны только проекты, где ваша текущая роль — редактор, менеджер или владелец.</p>}
      </fieldset>

      <fieldset>
        <legend>Проекты, доступные ключу <span>Выбрано: {selectedIds.length} из 20</span></legend>
        {selectableProjects.length ? <div className="mcp-persistent-projects">
          {projects.map(project => {
            const writable = canWrite(project);
            const checked = selectedIds.includes(project.id);
            return <label key={project.id} className="mcp-persistent-project">
              <input type="checkbox" checked={checked} disabled={busy || (mode === 'write' && !writable) || (!checked && selectedIds.length >= 20)} onChange={event => toggleProject(project.id, event.target.checked)} />
              <span>{project.name}{mode === 'write' && !writable ? ' · только чтение по вашей роли' : ''}</span>
            </label>;
          })}
        </div> : <p className="mcp-persistent-empty">{projects.length ? 'Нет активных проектов с ролью редактора или выше.' : 'Нет доступных активных проектов.'}</p>}
      </fieldset>

      <label className="mcp-persistent-label">Срок действия
        <select value={ttlDays} onChange={event => setTtlDays(Number(event.target.value))} disabled={busy}>
          <option value={1}>1 день</option><option value={7}>7 дней</option><option value={30}>30 дней</option>
        </select>
      </label>
      <label className="mcp-persistent-label">От чьего имени действует ключ
        <select value={serviceAccountId} onChange={event => setServiceAccountId(event.target.value)} disabled={busy || serviceAccountsEnabled === false}>
          <option value="">Моя учётная запись</option>
          {serviceAccounts.map(account => <option key={account.id} value={account.id}>{account.name}</option>)}
        </select>
      </label>
      <button className="mcp-persistent-primary" type="submit" disabled={busy || credentialsEnabled !== true || !name.trim() || name.trim().length > 80 || selectedIds.length < 1 || selectedIds.length > 20 || (mode === 'write' && selectedIds.some(id => !projectCanWrite(id)))}>
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
        <div className="mcp-persistent-card-content"><h4>{account.name}</h4><p><strong>ID:</strong> {account.id}</p><p><strong>Создана:</strong> <time dateTime={account.created_at}>{dateTime(account.created_at)}</time></p></div>
        {confirmDisableId === account.id ? <div className="mcp-persistent-confirm" role="group" aria-label={`Подтверждение отключения: ${account.name}`}>
          <span>Отключить навсегда и отозвать все связанные ключи?</span>
          <button type="button" className="mcp-persistent-danger" onClick={() => void disableServiceAccount(account.id)} disabled={busy}>Отключить</button>
          <button type="button" className="mcp-persistent-secondary" onClick={() => setConfirmDisableId('')} disabled={busy}>Отмена</button>
        </div> : <button type="button" className="mcp-persistent-danger" onClick={() => { setSecret(''); setConfirmDisableId(account.id); }} disabled={busy}><Trash2 size={15}/>Отключить</button>}
      </article>)}</div> : <p className="mcp-persistent-empty">Сервисных учётных записей пока нет.</p>}
    </section>

    <section className="mcp-persistent-list" aria-labelledby="mcp-persistent-list-title">
      <h3 id="mcp-persistent-list-title">Мои постоянные ключи</h3>
      {credentials.length ? <div className="mcp-persistent-cards">{credentials.map(credential => {
        const expired = Date.parse(credential.expires_at) <= Date.now();
        const revoked = Boolean(credential.revoked_at);
        return <article className="mcp-persistent-card" key={credential.id}>
          <div className="mcp-persistent-card-content">
            <h4>{credential.name}</h4>
            <p><strong>Статус:</strong> {revoked ? 'Отозван' : expired ? 'Истёк' : 'Активен'}</p>
            <p><strong>Режим:</strong> {credential.mode === 'write' ? 'Чтение и запись' : 'Только чтение'}</p>
            <p><strong>От имени:</strong> {credential.service_account_id ? serviceAccounts.find(account => account.id === credential.service_account_id)?.name || credential.service_account_id : 'Моя учётная запись'}</p>
            <p><strong>Проекты:</strong> {credential.project_ids.map(id => projectNames.get(id) || id).join(', ')}</p>
            <p><strong>Создан:</strong> <time dateTime={credential.created_at}>{dateTime(credential.created_at)}</time></p>
            <p><strong>Истекает:</strong> <time dateTime={credential.expires_at}>{dateTime(credential.expires_at)}</time></p>
          </div>
          {!revoked && !expired && <button type="button" className="mcp-persistent-danger" onClick={() => void revokeCredential(credential.id)} disabled={busy}><Trash2 size={15}/>Отозвать</button>}
        </article>;
      })}</div> : <p className="mcp-persistent-empty">Постоянных ключей пока нет.</p>}
    </section>
  </section>;
}
