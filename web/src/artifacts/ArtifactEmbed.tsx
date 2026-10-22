import { useEffect, useState } from 'react';
import { Box, Lock, ShieldAlert } from 'lucide-react';
import { client } from '../api';
import type { ArtifactDetail } from '../api';
import { artRouteUrl } from '../artifactsCore';
import { ArtifactFrame } from './ArtifactFrame';
import './artifacts.css';

export const ARTIFACT_NAVIGATE_EVENT = 'aiwiki:navigate-url';
/** Asks the application shell to open an in-app URL (so unsaved-changes checks and history stay consistent). */
export const requestNavigate = (url: string) => window.dispatchEvent(new CustomEvent(ARTIFACT_NAVIGATE_EVENT, { detail: { url } }));

/** Preview for a fenced ```artifact block in an article. */
export function ArtifactEmbed({ id }: { id: string }) {
  const [meta, setMeta] = useState<ArtifactDetail | null>(null);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    let live = true;
    setMeta(null); setFailed(false);
    client.artifact(id).then(value => { if (live) { if (value.deleted_at) setFailed(true); else setMeta(value); } }).catch(() => { if (live) setFailed(true); });
    return () => { live = false; };
  }, [id]);
  if (failed) return <div className="art-embed art-embed-unavailable" role="note"><ShieldAlert size={16} aria-hidden="true"/><span>Артефакт недоступен</span></div>;
  const href = artRouteUrl({ kind: 'page', id, mode: 'view', revision: null });
  return <figure className="art-embed">
    <figcaption><Box size={15} aria-hidden="true"/><strong>{meta?.title || 'Артефакт'}</strong>{meta?.visibility === 'private' && <Lock size={13} aria-label="Только владелец"/>}
      <a href={href} onClick={event => { if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return; event.preventDefault(); requestNavigate(href); }}>Открыть</a></figcaption>
    {meta ? <ArtifactFrame artifactId={id} title={meta.title} initialHeight={320} compact onUnavailable={() => setFailed(true)}/> : <div className="art-frame-loading" role="status">Загружаем артефакт…</div>}
  </figure>;
}
