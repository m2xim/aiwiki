import { useCallback, useEffect, useRef, useState } from 'react';
import { Copy, Pencil, RefreshCw, Trash2 } from 'lucide-react';
import { client, MCPCredential, MCPScopeInput, MCPServiceAccount } from './api';
import './MCPPersistentAccess.css';

const scopeErrors: Record<string, string> = {
  'invalid scope': 'Некорректный список доступа ключа. Проверьте строки: тип, объект и режим.',
  'scope access denied': 'Нет доступа к одному из выбранных объектов или нет прав редактора для режима «Запись».',
  'too many scope elements': 'Слишком много строк доступа: не больше 20.',
};

const explainError = (error: unknown) => {
  const message = error instanceof Error ? error.message : 'Не удалось выполнить запрос.';
  if (scopeErrors[message]) return scopeErrors[message];
  return message === 'Недостаточно прав для этого действия.'
    ? 'Сервер отказал в доступе. Проверьте свои права и войдите снова.'
    : message;
};

const MAX_SCOPE = 20;
type Target = { key: string; type: 'space' | 'project'; id: string; label: string; group: string; writable: boolean };
const targetKey = (type: string, id: string) => `${type}:${id}`;
const editorRole = (role?: string) => ['editor', 'manager', 'owner'].includes(role || '');

function ScopeEditor({ value, onChange, targets, disabled }: { value: MCPScopeInput[]; onChange: (next: MCPScopeInput[]) => void; targets: Target[]; disabled: boolean }) {
  const used = new Set(value.map(item => targetKey(item.type, item.id)));
  const free = targets.filter(target => !used.has(target.key));
  const groups = [...new Set(targets.map(target => target.group))];
  const update = (index: number, patch: Partial<MCPScopeInput>) => onChange(value.map((item, i) => i === index ? { ...item, ...patch } : item));
  return <fieldset className="mcp-persistent-scope">
    <legend>Доступ ключа <span>Строк: {value.length} из {MAX_SCOPE}</span></legend>
    {value.map((item, index) => {
      const key = targetKey(item.type, item.id);
      const known = targets.some(target => target.key === key);
      const writable = targets.find(target => target.key === key)?.writable ?? true;
      return <div className="mcp-persistent-scope-row" key={key}>
        <select aria-label="Пространство или проект" value={key} disabled={disabled} onChange={event => { const target = targets.find(t => t.key === event.target.value); if (target) update(index, { type: target.type, id: target.id, ...(target.writable ? {} : { mode: 'read' as const }) }); }}>
          {!known && <option value={key}>{item.id}</option>}
          {groups.map(group => <optgroup key={group} label={group}>
            {targets.filter(target => target.group === group && (target.key === key || !used.has(target.key))).map(target => <option key={target.key} value={target.key}>{target.label}</option>)}
          </optgroup>)}
        </select>
        <select aria-label="Режим" value={item.mode} disabled={disabled} onChange={event => update(index, { mode: event.target.value as 'read' | 'write' })}>
          <option value="read">Чтение</option>
          <option value="write" disabled={!writable} title={writable ? undefined : 'Нужны права редактора'}>Запись</option>
        </select>
        <button type="button" className="mcp-persistent-secondary" aria-label="Убрать строку" disabled={disabled} onClick={() => onChange(value.filter((_, i) => i !== index))}><Trash2 size={15}/></button>
      </div>;
    })}
    {!value.length && <p className="mcp-persistent-empty">Добавьте хотя бы одну строку.</p>}
    <div className="mcp-persistent-actions"><button type="button" className="mcp-persistent-secondary" disabled={disabled || value.length >= MAX_SCOPE || !free.length} onClick={() => onChange([...value, { type: free[0].type, id: free[0].id, mode: 'read' }])}>Добавить</button></div>
    <p className="mcp-persistent-hint">Общее пространство доступно ключу на чтение всегда. Чтобы ключ мог писать в него, добавьте его строкой с режимом «Запись». Проект включает все свои подпроекты. Режим «Запись» доступен только там, где у вас самих есть права редактора.</p>
  </fieldset>;
}

const dateTime = (value: string) => {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('ru-RU');
};

export default function MCPPersistentAccess() {
  const [targets, setTargets] = useState<Target[]>([]);
  const [credentials, setCredentials] = useState<MCPCredential[]>([]);
  const [serviceAccounts, setServiceAccounts] = useState<MCPServiceAccount[]>([]);
  const [credentialsEnabled, setCredentialsEnabled] = useState<boolean | null>(null);
  const [serviceAccountsEnabled, setServiceAccountsEnabled] = useState<boolean | null>(null);
  const [name, setName] = useState('');
  const [scope, setScope] = useState<MCPScopeInput[]>([]);
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
      const [credentialResult, accountResult, catalogSpaces] = await Promise.all([
        client.mcpCredentials(),
        client.mcpServiceAccounts(),
        client.catalog(),
      ]);
      if (!mounted.current) return;
      const list: Target[] = [];
      for (const space of catalogSpaces) {
        if (space.archived_at) continue;
        list.push({ key: targetKey('space', space.id), type: 'space', id: space.id, label: `${space.name} (всё пространство)`, group: space.name, writable: !space.effective_role || editorRole(space.effective_role) });
        const active = (space.projects || []).filter(project => !project.archived_at);
        const byParent = new Map<string, typeof active>();
        for (const project of active) byParent.set(project.parent_id || '', [...(byParent.get(project.parent_id || '') || []), project]);
        const ids = new Set(active.map(project => project.id));
        const add = (project: (typeof active)[number], depth: number) => list.push({ key: targetKey('project', project.id), type: 'project', id: project.id, label: `${'— '.repeat(depth)}${project.name}`, group: space.name, writable: editorRole(project.effective_role) });
        const walk = (parent: string, depth: number) => {
          for (const project of byParent.get(parent) || []) { add(project, depth); walk(project.id, depth + 1); }
        };
        walk('', 0);
        for (const project of active) if (project.parent_id && !ids.has(project.parent_id)) { add(project, 0); walk(project.id, 1); }
      }
      setCredentials(credentialResult.credentials || []);
      setCredentialsEnabled(credentialResult.enabled);
      setServiceAccounts((accountResult.service_accounts || []).filter(account => !account.disabled));
      setServiceAccountsEnabled(accountResult.enabled);
      setTargets(list);
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
    if (!trimmedName || trimmedName.length > 80 || !scope.length || scope.length > MAX_SCOPE || credentialsEnabled !== true) return;
    setSecret('');
    setError('');
    setNotice('');
    setBusy(true);
    try {
      const result = await client.createMCPCredential({
        name: trimmedName,
        scope,
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

  const [editing, setEditing] = useState<{ id: string; scope: MCPScopeInput[] } | null>(null);

  const startEdit = (credential: MCPCredential) => {
    setError('');
    setNotice('');
    setEditing({ id: credential.id, scope: credential.scope.map(({ type, id, mode }) => ({ type, id, mode })) });
  };

  const saveEdit = async () => {
    if (!editing || !editing.scope.length || editing.scope.length > MAX_SCOPE) return;
    setSecret('');
    setError('');
    setNotice('');
    setBusy(true);
    try {
      const updated = await client.updateMCPCredential(editing.id, { scope: editing.scope });
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
      <div><h2 id="mcp-persistent-title">Постоянный MCP-доступ</h2><p>Создавайте собственные ключи с ограниченным сроком и точным списком пространств и проектов.</p></div>
      <button type="button" className="mcp-persistent-secondary" onClick={() => void load()} disabled={busy} aria-label="Обновить постоянные ключи"><RefreshCw size={15}/>Обновить</button>
    </div>

    <aside className="mcp-persistent-warning">
      Каждый запрос проверяется по текущим правам владельца: ключ не даёт больше, чем есть у вас сейчас. Ключ действует только для выбранных пространств и проектов (проект включает свои подпроекты), плюс чтение общего пространства. Выход из системы не отзывает ключ. Ключ перестаёт работать после истечения срока, отзыва, отключения владельца администратором или если владелец не входил в вики больше 14 дней (снова заработает после входа). Максимальный срок — 180 дней.
    </aside>

    {credentialsEnabled === false && <div className="mcp-persistent-unavailable" role="status">Постоянный MCP-доступ недоступен в этом режиме работы вики.</div>}
    {serviceAccountsEnabled === false && <div className="mcp-persistent-unavailable" role="status">Управление сервисными учётными записями недоступно в этом режиме работы вики.</div>}
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
      <h3>Создать ключ</h3>
      <label className="mcp-persistent-label">Название ключа
        <input value={name} onChange={event => setName(event.target.value)} maxLength={80} required aria-describedby="mcp-persistent-name-limit" />
      </label>
      <small id="mcp-persistent-name-limit">До 80 символов</small>

      <ScopeEditor value={scope} onChange={setScope} targets={targets} disabled={busy}/>

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
      <button className="mcp-persistent-primary" type="submit" disabled={busy || credentialsEnabled !== true || !name.trim() || name.trim().length > 80 || scope.length < 1 || scope.length > MAX_SCOPE}>
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
            <div className="mcp-persistent-chips"><strong>Доступ:</strong>{(credential.scope || []).map(item => <span className="mcp-persistent-chip" key={`${item.type}:${item.id}`}>{item.name || item.id}{item.type === 'space' ? ' (пространство)' : ''} · {item.mode === 'write' ? 'запись' : 'чтение'}</span>)}</div>
            <p><strong>Создан:</strong> <time dateTime={credential.created_at}>{dateTime(credential.created_at)}</time></p>
            <p><strong>Истекает:</strong> <time dateTime={credential.expires_at}>{dateTime(credential.expires_at)}</time></p>
          </div>
          {editing?.id === credential.id && <div className="mcp-persistent-form mcp-persistent-edit">
            <ScopeEditor value={editing.scope} onChange={next => setEditing({ ...editing, scope: next })} targets={targets} disabled={busy}/>
            <div className="mcp-persistent-actions">
              <button type="button" className="mcp-persistent-primary" onClick={() => void saveEdit()} disabled={busy || editing.scope.length < 1 || editing.scope.length > MAX_SCOPE}>Сохранить</button>
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
