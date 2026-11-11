import { useEffect, useState } from 'react';
import { client, type Space } from './api';
import './artifacts/artifacts.css';

/** Administrator choice of the "general" space (GET/PUT /api/admin/settings, field general_space_id). */
export function GeneralSpaceSettings() {
  const [spaces, setSpaces] = useState<Space[] | null>(null);
  const [value, setValue] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [saved, setSaved] = useState(false);
  useEffect(() => {
    let live = true;
    Promise.all([client.spaces(), client.adminSettings()])
      .then(([list, settings]) => { if (live) { setSpaces(list.filter(space => !space.archived_at)); setValue(settings.general_space_id ?? ''); } })
      .catch(e => { if (live) setError((e as Error).message); });
    return () => { live = false; };
  }, []);
  const change = async (next: string) => {
    setBusy(true); setError(''); setSaved(false);
    try { const result = await client.setAdminSettings({ general_space_id: next }); setValue(result?.general_space_id ?? next); setSaved(true); }
    catch (e) { setError((e as Error).message); }
    finally { setBusy(false); }
  };
  const known = spaces?.some(space => space.id === value) ?? false;
  const loading = spaces === null || value === null;
  return <section className="art-settings" aria-labelledby="general-space-title">
    <h2 id="general-space-title">Общее пространство</h2>
    <label className="art-settings-row">
      <span><b>Пространство общих правил</b>
        <select value={known ? value ?? '' : ''} disabled={loading || busy} onChange={event => void change(event.target.value)}>
          {!known && <option value="" disabled>— не выбрано —</option>}
          {(spaces ?? []).map(space => <option key={space.id} value={space.id}>{space.name}</option>)}
        </select>
        <small>Закреплённые статьи этого пространства получают все агенты при подключении — первым слоем правил. Все пользователи и все MCP-ключи читают это пространство целиком; писать в него могут только ключи в режиме записи у пользователей с ролью редактора или выше в этом пространстве.</small></span>
    </label>
    {loading && !error && <p className="art-muted" role="status">Загружаем…</p>}
    {saved && <p className="art-ok" role="status">Общее пространство сохранено.</p>}
    {error && <p className="art-error-text" role="alert">{error}</p>}
  </section>;
}
