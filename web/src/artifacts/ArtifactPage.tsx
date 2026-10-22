import { useCallback, useEffect, useRef, useState } from 'react';
import { ArrowLeft, Box, Clock3, Files, History, Lock, Maximize2, Minimize2, Pencil, RotateCcw, Share2, Trash2, Users } from 'lucide-react';
import { client } from '../api';
import type { ArtifactDetail, ArtifactRevisionInfo, Space } from '../api';
import { visibilityLabel, type ArtRoute, type Visibility } from '../artifactsCore';
import { ArtifactEditor } from './ArtifactEditor';
import { ArtifactFrame } from './ArtifactFrame';
import { filesLabel } from './ArtifactList';
import { Modal } from './Modal';
import { ShareDialog } from './ShareDialog';

const formatDate = (value?: string) => value ? new Intl.DateTimeFormat('ru-RU', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value)) : '';

type Props = {
  id: string; mode: 'view' | 'edit' | 'history'; revision: number | null;
  spaces: Space[]; theme: 'light' | 'dark';
  go: (route: ArtRoute, replace?: boolean) => void;
  backToList: () => void;
  onLoaded: (artifact: ArtifactDetail) => void;
  onDirty: (dirty: boolean) => void;
  onChanged: () => void;
};

export function ArtifactPage({ id, mode, revision, spaces, theme, go, backToList, onLoaded, onDirty, onChanged }: Props) {
  const [artifact, setArtifact] = useState<ArtifactDetail | null>(null);
  const [error, setError] = useState('');
  const [deleted, setDeleted] = useState(false);
  const [shareOpen, setShareOpen] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState('');
  const [full, setFull] = useState(false);
  const [history, setHistory] = useState<ArtifactRevisionInfo[] | null>(null);
  const [historyError, setHistoryError] = useState('');
  const loadedRef = useRef(onLoaded); loadedRef.current = onLoaded;

  const load = useCallback(async () => {
    try {
      const value = await client.artifact(id);
      setArtifact(value); setDeleted(Boolean(value.deleted_at)); setError('');
      loadedRef.current(value);
      return value;
    } catch (e) { setError((e as Error).message); return null; }
  }, [id]);
  useEffect(() => { setArtifact(null); setError(''); setHistory(null); setFull(false); void load(); }, [load]);
  useEffect(() => {
    if (mode !== 'history') return;
    let live = true; setHistoryError('');
    client.artifactHistory(id).then(list => { if (live) setHistory([...list].sort((a, b) => b.revision - a.revision)); }).catch(e => { if (live) { setHistory([]); setHistoryError((e as Error).message); } });
    return () => { live = false; };
  }, [id, mode, artifact?.revision]);
  useEffect(() => {
    if (!full) return;
    const onKey = (event: KeyboardEvent) => { if (event.key === 'Escape' && !document.querySelector('[role="dialog"]')) setFull(false); };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [full]);

  if (error && !artifact) return <section className="art-page"><div className="art-top"><button type="button" className="button ghost" onClick={backToList}><ArrowLeft size={16}/>К списку</button></div>
    <div className="art-alert error" role="alert">Артефакт недоступен: {error}</div></section>;
  if (!artifact) return <section className="art-page"><p className="art-muted" role="status">Загружаем артефакт…</p></section>;

  const page = (m: 'view' | 'edit' | 'history', rev: number | null = null, replace = false) => go({ kind: 'page', id, mode: m, revision: rev }, replace);
  const remove = async () => {
    setBusy(true); setActionError('');
    try {
      await client.deleteArtifact(id, artifact.revision);
      setConfirmDelete(false); onChanged();
      const fresh = await load(); // a deleted artifact may be hidden by the server; the local flag keeps the page usable
      setDeleted(true); if (!fresh) setArtifact({ ...artifact, deleted_at: new Date().toISOString() });
    } catch (e) { setActionError((e as Error).message); setConfirmDelete(false); }
    finally { setBusy(false); }
  };
  const restore = async () => {
    setBusy(true); setActionError('');
    try {
      const fresh = await load();
      await client.restoreArtifact(id, (fresh || artifact).revision);
      onChanged(); setDeleted(false); await load();
    } catch (e) { setActionError((e as Error).message); }
    finally { setBusy(false); }
  };
  const changeVisibility = async (visibility: Visibility) => {
    const saved = await client.updateArtifact(id, { base_revision: artifact.revision, visibility, message: 'Изменена видимость' });
    setArtifact(saved); onChanged();
  };

  if (mode === 'edit' && artifact.can_edit && !deleted) return <ArtifactEditor key={`${artifact.id}:${artifact.revision}`} artifact={artifact} spaces={spaces} initialSpace={artifact.space_id} initialProject={artifact.project_id || ''} theme={theme}
    onDirty={onDirty} onCancel={() => page('view')} onReload={() => { void load(); }}
    onSaved={saved => { setArtifact(saved); onChanged(); page('view', null, true); }}/>;

  const pinned = mode === 'history' ? revision : null;
  return <section className={`art-page art-view${full ? ' art-fullscreen' : ''}`}>
    {!full && <>
      <div className="art-top"><button type="button" className="button ghost" onClick={backToList}><ArrowLeft size={16}/>К списку</button><span className="eyebrow">АРТЕФАКТ</span></div>
      <div className="art-title-row">
        <div>
          <h1>{artifact.title || 'Без названия'}</h1>
          {artifact.description && <p className="art-description">{artifact.description}</p>}
          <p className="art-meta">
            <span className={`art-badge ${artifact.visibility === 'private' ? 'private' : ''}`}>{artifact.visibility === 'private' ? <Lock size={12} aria-hidden="true"/> : <Users size={12} aria-hidden="true"/>}{visibilityLabel(artifact.visibility)}</span>
            <span><Box size={12} aria-hidden="true"/> версия {artifact.revision}</span>
            <span><Files size={12} aria-hidden="true"/> {filesLabel(artifact.file_count ?? artifact.manifest.length)}</span>
            {(artifact.owner_name || artifact.author) && <span>{artifact.owner_name || artifact.author}</span>}
            <span><Clock3 size={12} aria-hidden="true"/> <time dateTime={artifact.updated_at}>{formatDate(artifact.updated_at)}</time></span>
          </p>
        </div>
        <div className="art-toolbar">
          {!deleted && artifact.can_edit && <button type="button" className="button primary" onClick={() => page('edit')}><Pencil size={15}/>Редактировать</button>}
          {!deleted && artifact.can_share && <button type="button" className="button" onClick={() => setShareOpen(true)}><Share2 size={15}/>Поделиться</button>}
          <button type="button" className={`button${mode === 'history' ? ' active' : ''}`} onClick={() => page(mode === 'history' ? 'view' : 'history')} aria-pressed={mode === 'history'}><History size={15}/>История</button>
          {!deleted && <button type="button" className="button" onClick={() => setFull(true)}><Maximize2 size={15}/>Открыть во весь экран</button>}
          {!deleted && artifact.can_edit && <button type="button" className="button ghost art-danger-text" onClick={() => setConfirmDelete(true)} disabled={busy}><Trash2 size={15}/>Удалить</button>}
        </div>
      </div>
      {mode === 'edit' && !artifact.can_edit && <div className="art-alert warn" role="status">У вас нет прав на редактирование этого артефакта.</div>}
      {deleted && <div className="art-alert warn" role="status"><span>Артефакт удалён. Публичные ссылки на него отозваны.</span>{artifact.can_edit && <button type="button" className="button small" onClick={() => void restore()} disabled={busy}><RotateCcw size={14}/>{busy ? 'Восстанавливаем…' : 'Восстановить'}</button>}</div>}
      {actionError && <div className="art-alert error" role="alert">{actionError}</div>}
      {mode === 'history' && <div className="art-history">
        <h2>История версий</h2>
        {history === null ? <p className="art-muted" role="status">Загружаем…</p> : historyError ? <div className="art-alert error" role="alert">{historyError}</div> : <ul>
          {history.map(item => <li key={item.revision} className={(pinned ?? artifact.revision) === item.revision ? 'selected' : ''}>
            <button type="button" onClick={() => page('history', item.revision === artifact.revision ? null : item.revision)} aria-current={(pinned ?? artifact.revision) === item.revision ? 'true' : undefined}>
              <b>Версия {item.revision}{item.revision === artifact.revision && <small> ТЕКУЩАЯ</small>}</b>
              <span>{item.message || 'Без комментария'}</span>
              <small>{item.author || 'Автор неизвестен'} · {formatDate(item.created_at)} · {filesLabel(item.file_count)}</small>
            </button></li>)}
        </ul>}
        {pinned && <p className="art-muted">Показана версия {pinned}. <button type="button" className="link-button" onClick={() => page('history')}>Вернуться к текущей</button></p>}
      </div>}
    </>}
    {!deleted && <div className="art-viewer">
      {full && <button type="button" className="button art-full-close" onClick={() => setFull(false)}><Minimize2 size={15}/>Закрыть полноэкранный режим</button>}
      <ArtifactFrame key={`${id}:${pinned ?? 'current'}:${artifact.revision}`} artifactId={id} revision={pinned} title={artifact.title} sizing={full ? 'fill' : 'auto'}/>
    </div>}

    {shareOpen && <ShareDialog artifact={artifact} onClose={() => setShareOpen(false)} onVisibility={changeVisibility}/>}
    {confirmDelete && <Modal title="Удалить артефакт?" onClose={() => setConfirmDelete(false)} busy={busy}>
      <p>«{artifact.title}» исчезнет из списка, а все публичные ссылки на него будут отозваны. Удаление мягкое: артефакт можно будет восстановить на его странице.</p>
      <div className="art-actions"><button type="button" className="button ghost" onClick={() => setConfirmDelete(false)} disabled={busy}>Отмена</button><button type="button" className="button art-danger" onClick={() => void remove()} disabled={busy}>{busy ? 'Удаляем…' : 'Удалить'}</button></div>
    </Modal>}
  </section>;
}
