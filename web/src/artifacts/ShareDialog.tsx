import { useCallback, useEffect, useRef, useState } from 'react';
import { Copy, Link2, Lock, Trash2, Users } from 'lucide-react';
import { client } from '../api';
import type { ArtifactDetail, ArtifactShare } from '../api';
import { publicLinksDisabledHint, shareLabel, type Visibility } from '../artifactsCore';
import { Modal } from './Modal';

const formatDate = (value: string) => value ? new Intl.DateTimeFormat('ru-RU', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value)) : '—';

type Props = { artifact: ArtifactDetail; onClose: () => void; onVisibility: (visibility: Visibility) => Promise<void> };

export function ShareDialog({ artifact, onClose, onVisibility }: Props) {
  const [shares, setShares] = useState<ArtifactShare[] | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [pin, setPin] = useState(false);
  const [created, setCreated] = useState<{ url: string; revision: number | null } | null>(null);
  const [copyState, setCopyState] = useState<'' | 'done' | 'manual'>('');
  const [revoking, setRevoking] = useState<ArtifactShare | null>(null);
  const urlRef = useRef<HTMLInputElement>(null);
  const reload = useCallback(async () => {
    try { setShares(await client.artifactShares(artifact.id)); setError(''); } catch (e) { setError((e as Error).message); setShares([]); }
  }, [artifact.id]);
  useEffect(() => { void reload(); }, [reload]);

  const create = async () => {
    setBusy(true); setError(''); setCreated(null); setCopyState('');
    try {
      const share = await client.createArtifactShare(artifact.id, pin ? artifact.revision : null);
      setCreated({ url: share.url || '', revision: share.revision ?? null });
      await reload();
    } catch (e) { const text = (e as Error).message; setError(publicLinksDisabledHint(text) || text); }
    finally { setBusy(false); }
  };
  const copy = async () => {
    try { await navigator.clipboard.writeText(created?.url || ''); setCopyState('done'); }
    catch { setCopyState('manual'); urlRef.current?.focus(); urlRef.current?.select(); }
  };
  const revoke = async () => {
    if (!revoking) return;
    setBusy(true); setError('');
    try { await client.revokeArtifactShare(artifact.id, revoking.id); setRevoking(null); await reload(); }
    catch (e) { setError((e as Error).message); }
    finally { setBusy(false); }
  };
  const changeVisibility = async (visibility: Visibility) => {
    setBusy(true); setError('');
    try { await onVisibility(visibility); } catch (e) { setError((e as Error).message); }
    finally { setBusy(false); }
  };

  const active = (shares || []).filter(share => !share.revoked_at);
  return <Modal title="Поделиться артефактом" onClose={onClose} busy={busy} wide>
    {error && <div className="art-alert error" role="alert">{error}</div>}
    <section className="art-share-section">
      <h3>Кто видит внутри вики</h3>
      {artifact.can_manage ? <div className="art-radio-group" role="radiogroup" aria-label="Видимость">
        <label className={artifact.visibility === 'inherit' ? 'selected' : ''}><input type="radio" name="art-visibility" checked={artifact.visibility === 'inherit'} disabled={busy} onChange={() => void changeVisibility('inherit')}/><Users size={15} aria-hidden="true"/><span><b>Видно участникам пространства</b><small>Читатели пространства или проекта могут открыть артефакт, редакторы — изменить его.</small></span></label>
        <label className={artifact.visibility === 'private' ? 'selected' : ''}><input type="radio" name="art-visibility" checked={artifact.visibility === 'private'} disabled={busy} onChange={() => void changeVisibility('private')}/><Lock size={15} aria-hidden="true"/><span><b>Только мне</b><small>Открыть и изменить может только владелец; менеджеры и администраторы доступа не получают.</small></span></label>
      </div> : <p className="art-muted">{artifact.visibility === 'private' ? <Lock size={13} aria-hidden="true"/> : <Users size={13} aria-hidden="true"/>} {artifact.visibility === 'private' ? 'Только владелец' : 'Участники пространства'}. Менять видимость может владелец или менеджер.</p>}
    </section>
    <section className="art-share-section">
      <h3>Публичные ссылки</h3>
      <div className="art-alert warn" role="note">Любой, у кого есть ссылка, сможет открыть артефакт без входа.</div>
      <div className="art-share-create">
        <label><input type="checkbox" checked={pin} onChange={event => setPin(event.target.checked)}/> Закрепить текущую версию ({artifact.revision})</label>
        <small className="art-muted">{pin ? `Ссылка всегда будет показывать версию ${artifact.revision}, даже после правок.` : 'Ссылка всегда показывает последнюю версию.'}</small>
        <button type="button" className="button primary" onClick={() => void create()} disabled={busy}><Link2 size={15}/>{busy ? 'Создаём…' : 'Создать ссылку'}</button>
      </div>
      {created && <div className="art-created" role="status">
        <b>Ссылка создана ({shareLabel(created.revision)}). Адрес показывается только сейчас: скопируйте его.</b>
        <div className="art-created-row"><input ref={urlRef} readOnly value={created.url} aria-label="Публичная ссылка" onFocus={event => event.currentTarget.select()}/><button type="button" className="button" onClick={() => void copy()}><Copy size={14}/>Копировать</button></div>
        {copyState === 'done' && <small className="art-ok">Скопировано.</small>}
        {copyState === 'manual' && <small>Не удалось скопировать автоматически — адрес выделен, нажмите Ctrl+C.</small>}
      </div>}
      <h4>Действующие ссылки</h4>
      {shares === null ? <p className="art-muted">Загружаем…</p> : !active.length ? <p className="art-muted">Публичных ссылок пока нет.</p> : <ul className="art-share-list">
        {active.map(share => <li key={share.id}><span><b>{share.revision ? `Версия ${share.revision}` : 'Последняя версия'}</b><small>{formatDate(share.created_at)}{share.created_by ? ` · ${share.created_by}` : ''}</small></span>
          <button type="button" className="button ghost small" onClick={() => setRevoking(share)} disabled={busy} aria-label={`Отозвать ссылку от ${formatDate(share.created_at)}`}><Trash2 size={14}/>Отозвать</button></li>)}
      </ul>}
    </section>
    {revoking && <Modal title="Отозвать ссылку?" onClose={() => setRevoking(null)} busy={busy}>
      <p>Ссылка от {formatDate(revoking.created_at)} ({shareLabel(revoking.revision)}) перестанет открываться. Отменить это нельзя, но можно создать новую.</p>
      <div className="art-actions"><button type="button" className="button ghost" onClick={() => setRevoking(null)} disabled={busy}>Отмена</button><button type="button" className="button art-danger" onClick={() => void revoke()} disabled={busy}>{busy ? 'Отзываем…' : 'Отозвать'}</button></div>
    </Modal>}
  </Modal>;
}
