import { useEffect, useState } from 'react';
import { client } from '../api';
import './artifacts.css';

/** Administrator switch for organisation-wide public links (GET/PUT /api/admin/settings). */
export function ArtifactSettings() {
  const [enabled, setEnabled] = useState<boolean | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [saved, setSaved] = useState(false);
  useEffect(() => {
    let live = true;
    client.adminSettings().then(value => { if (live) setEnabled(Boolean(value.public_artifact_links)); }).catch(e => { if (live) setError((e as Error).message); });
    return () => { live = false; };
  }, []);
  const change = async (next: boolean) => {
    setBusy(true); setError(''); setSaved(false);
    try { const value = await client.setAdminSettings({ public_artifact_links: next }); setEnabled(Boolean(value?.public_artifact_links ?? next)); setSaved(true); }
    catch (e) { setError((e as Error).message); }
    finally { setBusy(false); }
  };
  return <section className="art-settings" aria-labelledby="art-settings-title">
    <h2 id="art-settings-title">Артефакты</h2>
    <label className="art-settings-row">
      <input type="checkbox" checked={enabled === true} disabled={enabled === null || busy} onChange={event => void change(event.target.checked)}/>
      <span><b>Разрешить публичные ссылки на артефакты</b>
        <small>Любой, у кого есть публичная ссылка, открывает артефакт без входа в вики. Если выключить, существующие ссылки перестанут работать, но не удалятся: после повторного включения они снова откроются.</small></span>
    </label>
    {enabled === null && !error && <p className="art-muted" role="status">Загружаем…</p>}
    {saved && <p className="art-ok" role="status">{enabled ? 'Публичные ссылки разрешены.' : 'Публичные ссылки выключены.'}</p>}
    {error && <p className="art-error-text" role="alert">{error}</p>}
  </section>;
}
