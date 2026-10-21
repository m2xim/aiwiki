import { useCallback, useEffect, useState } from 'react';
import { Copy, RefreshCw, Trash2 } from 'lucide-react';
import { client, MCPDelegation, Project } from './api';
import './MCPDelegations.css';

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

export default function MCPDelegations() {
  const [projects, setProjects] = useState<Project[]>([]);
  const [delegations, setDelegations] = useState<MCPDelegation[]>([]);
  const [name, setName] = useState('');
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [token, setToken] = useState('');
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    setBusy(true);
    setError('');
    try {
      const [availableProjects, currentDelegations] = await Promise.all([
        client.projects(),
        client.mcpDelegations(),
      ]);
      const activeProjects = availableProjects.filter(project => !project.archived_at);
      setProjects(activeProjects);
      setDelegations(currentDelegations);
      const allowed = new Set(activeProjects.map(project => project.id));
      setSelectedIds(current => current.filter(id => allowed.has(id)));
    } catch (reason) {
      setError(explainError(reason));
    } finally {
      setBusy(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  const toggleProject = (id: string, checked: boolean) => {
    setSelectedIds(current => {
      if (checked) return current.length < 20 && !current.includes(id) ? [...current, id] : current;
      return current.filter(item => item !== id);
    });
  };

  const create = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!name.trim() || name.trim().length > 80 || selectedIds.length < 1 || selectedIds.length > 20) return;
    setBusy(true);
    setError('');
    setNotice('');
    setToken('');
    try {
      const result = await client.createMCPDelegation({ name: name.trim(), project_ids: selectedIds, ttl_seconds: 300 });
      setToken(result.token);
      setName('');
      await load();
    } catch (reason) {
      setError(explainError(reason));
    } finally {
      setBusy(false);
    }
  };

  const revoke = async (id: string) => {
    setBusy(true);
    setError('');
    setNotice('');
    try {
      await client.revokeMCPDelegation(id);
      await load();
    } catch (reason) {
      setError(explainError(reason));
    } finally {
      setBusy(false);
    }
  };

  const copyToken = async () => {
    try {
      await navigator.clipboard.writeText(token);
      setNotice('Секрет скопирован в буфер обмена.');
    } catch {
      setError('Не удалось скопировать секрет. Вы можете выделить его и скопировать вручную.');
    }
  };

  return <section className="mcp-delegations" aria-labelledby="mcp-delegations-title" aria-busy={busy}>
    <div className="mcp-delegations-heading">
      <div><h2 id="mcp-delegations-title">Временный MCP-доступ</h2><p>Выдайте агенту временный ключ для чтения выбранных проектов.</p></div>
      <button type="button" className="mcp-delegations-secondary" onClick={() => void load()} disabled={busy} aria-label="Обновить ключи"><RefreshCw size={15}/>Обновить</button>
    </div>

    <aside className="mcp-delegations-warning">
      <strong>Только чтение и только выбранные проекты.</strong> Доступ распространяется ровно на отмеченные проекты; дочерние проекты автоматически не включаются. Статьи в корне пространств и общие инструкции не входят в доступ. Запрашивается срок 5 минут, фактическое время окончания задаёт сервер. Завершение родительской браузерной сессии, выход из системы или перезапуск сервера отзывает доступ.
    </aside>

    {error && <div className="mcp-delegations-error" role="alert">{error}</div>}
    {notice && <div className="mcp-delegations-status" role="status">{notice}</div>}

    <form className="mcp-delegations-form" onSubmit={event => void create(event)}>
      <label className="mcp-delegations-name">Название ключа
        <input value={name} onChange={event => setName(event.target.value)} maxLength={80} required aria-describedby="mcp-name-limit" />
      </label>
      <small id="mcp-name-limit">До 80 символов</small>
      <fieldset>
        <legend>Проекты, доступные ключу <span>Выбрано: {selectedIds.length} из 20</span></legend>
        {projects.length ? <div className="mcp-delegations-projects">
          {projects.map(project => <label key={project.id} className="mcp-delegations-project">
            <input type="checkbox" checked={selectedIds.includes(project.id)} disabled={busy || (!selectedIds.includes(project.id) && selectedIds.length >= 20)} onChange={event => toggleProject(project.id, event.target.checked)} />
            <span>{project.name}</span>
          </label>)}
        </div> : <p className="mcp-delegations-empty">Нет доступных проектов.</p>}
      </fieldset>
      <button className="mcp-delegations-primary" type="submit" disabled={busy || !name.trim() || name.trim().length > 80 || selectedIds.length < 1 || projects.length === 0}>Создать ключ</button>
    </form>

    {token && <section className="mcp-delegations-secret" aria-labelledby="mcp-secret-title">
      <h3 id="mcp-secret-title">Секрет показан только сейчас</h3>
      <p>Скопируйте его и сохраните в подключении MCP. После очистки показать секрет снова будет нельзя.</p>
      <textarea aria-label="Временный ключ MCP" readOnly value={token} rows={3} spellCheck={false} />
      <div className="mcp-delegations-actions">
        <button type="button" className="mcp-delegations-secondary" onClick={() => void copyToken()}><Copy size={15}/>Скопировать</button>
        <button type="button" className="mcp-delegations-secondary" onClick={() => { setToken(''); setNotice('Секрет очищен.'); }}>Очистить секрет</button>
      </div>
    </section>}

    <section className="mcp-delegations-list" aria-labelledby="mcp-list-title">
      <h3 id="mcp-list-title">Ваши активные ключи</h3>
      {delegations.length ? <div className="mcp-delegations-cards">{delegations.map(delegation => {
        const projectNames = new Map(projects.map(project => [project.id, project.name]));
        return <article className="mcp-delegation-card" key={delegation.id}>
          <div className="mcp-delegation-card-content">
            <h4>{delegation.name}</h4>
            <p><strong>Проекты:</strong> {delegation.project_ids.map(id => projectNames.get(id) || id).join(', ')}</p>
            <p><strong>Создана:</strong> <time dateTime={delegation.created_at}>{dateTime(delegation.created_at)}</time></p>
            <p><strong>Истекает:</strong> <time dateTime={delegation.expires_at}>{dateTime(delegation.expires_at)}</time></p>
          </div>
          <button type="button" className="mcp-delegations-revoke" onClick={() => void revoke(delegation.id)} disabled={busy}><Trash2 size={15}/>Отозвать</button>
        </article>;
      })}</div> : <p className="mcp-delegations-empty">Активных ключей нет.</p>}
    </section>
  </section>;
}
