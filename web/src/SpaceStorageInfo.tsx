import { useEffect, useState } from 'react';
import { client, SpaceStorage } from './api';
import './SpaceStorage.css';

const MB = 1024 * 1024;
export const formatBytes = (value: number) => {
  if (value < 1024) return `${value} Б`;
  if (value < MB) return `${(value / 1024).toFixed(1)} КБ`;
  if (value < 1024 * MB) return `${(value / MB).toFixed(1)} МБ`;
  return `${(value / (1024 * MB)).toFixed(2)} ГБ`;
};

// Used storage and limit of a space. The server answers managers, owners and
// system administrators only; for everybody else this renders nothing.
export function SpaceStorageInfo({ spaceId, admin }: { spaceId: string; admin: boolean }) {
  const [storage, setStorage] = useState<SpaceStorage | null>(null);
  const [limitText, setLimitText] = useState('');
  const [pending, setPending] = useState(false);
  const [error, setError] = useState('');
  const load = () => client.spaceStorage(spaceId).then(value => { setStorage(value); setLimitText(value.override === null ? '' : String(Math.round(value.override / MB * 100) / 100)); }).catch(() => setStorage(null));
  useEffect(() => { setStorage(null); void load(); }, [spaceId]);
  if (!storage) return null;
  const unlimited = storage.limit === 0;
  const percent = unlimited ? 0 : Math.min(100, Math.round(storage.used / storage.limit * 100));
  const apply = async () => {
    const text = limitText.trim().replace(',', '.');
    const megabytes = text === '' ? null : Number(text);
    if (megabytes !== null && (!Number.isFinite(megabytes) || megabytes < 0)) { setError('Введите неотрицательное число мегабайт.'); return; }
    setPending(true); setError('');
    try { await client.setSpaceQuota(spaceId, megabytes === null ? null : Math.round(megabytes * MB)); await load(); }
    catch (e) { setError((e as Error).message); } finally { setPending(false); }
  };
  return <div className="space-storage" aria-label="Хранилище пространства">
    <strong>Хранилище</strong>
    <p>{unlimited ? `Занято ${formatBytes(storage.used)}, без ограничения` : `Занято ${formatBytes(storage.used)} из ${formatBytes(storage.limit)}`}{storage.override === null && !unlimited ? ' (лимит по умолчанию)' : ''}</p>
    {!unlimited && <div className={`space-storage-bar${storage.used > storage.limit ? ' is-over' : ''}`} role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={percent}><span style={{ width: `${percent}%` }}/></div>}
    {!unlimited && storage.used > storage.limit && <p role="status">Лимит превышен: можно только удалять материалы.</p>}
    {admin && <div className="space-storage-admin">
      <label className="field"><span>Лимит, МБ (пусто — по умолчанию, 0 — без ограничения)</span><input inputMode="decimal" value={limitText} onChange={e => setLimitText(e.target.value)} onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); e.stopPropagation(); void apply(); } }} disabled={pending}/></label>
      <button type="button" className="button ghost small" disabled={pending} onClick={() => void apply()}>{pending ? 'Сохраняем…' : 'Применить лимит'}</button>
    </div>}
    {error && <p role="alert" className="spaces-error">{error}</p>}
  </div>;
}
