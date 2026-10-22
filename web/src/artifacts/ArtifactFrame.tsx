import { useCallback, useEffect, useRef, useState } from 'react';
import { RefreshCw } from 'lucide-react';
import { client } from '../api';
import { FRAME_SANDBOX, parseResizeMessage, refreshDelay } from '../artifactsCore';

type Props = {
  artifactId: string;
  revision?: number | null;
  title: string;
  /** auto: height follows aiwiki:resize messages; fill: occupies the whole parent. */
  sizing?: 'auto' | 'fill';
  initialHeight?: number;
  /** Called when the capability request fails (no access, deleted, ...). */
  onUnavailable?: (message: string) => void;
  /** Hide the reload button (embeds in articles). */
  compact?: boolean;
};

/** Sandboxed iframe on /artifact-frame/<capability>/. Never allow-same-origin: the artifact must not get the wiki's origin. */
export function ArtifactFrame({ artifactId, revision = null, title, sizing = 'auto', initialHeight = 420, onUnavailable, compact = false }: Props) {
  const [url, setUrl] = useState('');
  const [error, setError] = useState('');
  const [height, setHeight] = useState(initialHeight);
  const [nonce, setNonce] = useState(0);
  const frameRef = useRef<HTMLIFrameElement>(null);
  const unavailableRef = useRef(onUnavailable); unavailableRef.current = onUnavailable;

  // Capability: request on mount / artifact / revision / manual reload, renew one minute before it expires.
  useEffect(() => {
    let live = true; let timer = 0;
    const load = async (first: boolean) => {
      try {
        const view = await client.artifactView(artifactId, revision);
        if (!live) return;
        setUrl(view.frame_url); setError('');
        timer = window.setTimeout(() => void load(false), refreshDelay(view.expires_at, Date.now()));
      } catch (e) {
        if (!live) return;
        const message = (e as Error).message;
        if (first) { setUrl(''); setError(message); unavailableRef.current?.(message); }
        else timer = window.setTimeout(() => void load(false), 30000); // keep the current frame, try again soon
      }
    };
    setError(''); if (nonce === 0) setUrl('');
    void load(true);
    return () => { live = false; window.clearTimeout(timer); };
  }, [artifactId, revision, nonce]);

  // Auto height: accept only the iframe's own window.
  useEffect(() => {
    if (sizing !== 'auto') return;
    const onMessage = (event: MessageEvent) => {
      const next = parseResizeMessage(event, frameRef.current?.contentWindow);
      if (next !== null) setHeight(next);
    };
    window.addEventListener('message', onMessage);
    return () => window.removeEventListener('message', onMessage);
  }, [sizing]);

  const reload = useCallback(() => setNonce(value => value + 1), []);
  if (error) return <div className="art-frame-error" role="alert">{error}</div>;
  if (!url) return <div className="art-frame-loading" role="status">Загружаем артефакт…</div>;
  return <div className={`art-frame-box ${sizing}`}>
    <iframe ref={frameRef} className="art-frame" title={title || 'Артефакт'} src={url} sandbox={FRAME_SANDBOX} referrerPolicy="no-referrer" style={sizing === 'auto' ? { height } : undefined}/>
    {!compact && <button type="button" className="icon-button art-frame-reload" onClick={reload} title="Перезагрузить артефакт" aria-label="Перезагрузить артефакт"><RefreshCw size={14}/></button>}
  </div>;
}
