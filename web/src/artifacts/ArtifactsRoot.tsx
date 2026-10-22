import { useEffect, useState } from 'react';
import { client } from '../api';
import type { ArtifactDetail, Space } from '../api';
import type { ArtRoute } from '../artifactsCore';
import { ArtifactEditor } from './ArtifactEditor';
import { ArtifactList } from './ArtifactList';
import { ArtifactPage } from './ArtifactPage';
import './artifacts.css';

export { artRouteUrl, parseArtRoute, type ArtRoute } from '../artifactsCore';

/** Number of artifacts in the space for the sidebar badge; null while unknown or when the server has no such endpoint. */
export function useArtifactCount(spaceId: string, reloadKey: number): number | null {
  const [count, setCount] = useState<number | null>(null);
  useEffect(() => {
    if (!spaceId) { setCount(null); return; }
    let live = true;
    client.artifacts({ space_id: spaceId }).then(list => { if (live) setCount(list.length); }).catch(() => { if (live) setCount(null); });
    return () => { live = false; };
  }, [spaceId, reloadKey]);
  return count;
}

type Props = {
  route: ArtRoute;
  spaceId: string;
  spaces: Space[];
  theme: 'light' | 'dark';
  /** Navigate inside the application (the shell asks about unsaved changes and writes the URL). */
  go: (route: ArtRoute, replace?: boolean) => void;
  onDirty: (dirty: boolean) => void;
  onChanged: () => void;
  /** An artifact opened by link belongs to some space: the sidebar follows it. */
  onSpace: (spaceId: string) => void;
};

export function ArtifactsRoot({ route, spaceId, spaces, theme, go, onDirty, onChanged, onSpace }: Props) {
  const spaceName = spaces.find(space => space.id === spaceId)?.name || spaceId;
  const list = (): ArtRoute => ({ kind: 'list', space: spaceId, q: '', project: '' });
  useEffect(() => { document.title = route.kind === 'list' ? `Артефакты · ${spaceName} — AI Wiki` : 'Артефакт — AI Wiki'; }, [route.kind, spaceName]);
  if (route.kind === 'list') return <ArtifactList spaceId={spaceId} spaceName={spaceName} q={route.q} project={route.project} reloadKey={0}
    onFilters={(q, project) => go({ kind: 'list', space: spaceId, q, project }, true)}
    onOpen={id => go({ kind: 'page', id, mode: 'view', revision: null })}
    onCreate={() => go({ kind: 'new', space: spaceId, project: route.project })}/>;
  if (route.kind === 'new') return <ArtifactEditor spaces={spaces} initialSpace={route.space || spaceId} initialProject={route.project} theme={theme}
    onDirty={onDirty} onCancel={() => go(list())} onReload={() => undefined}
    onSaved={(saved: ArtifactDetail) => { onChanged(); go({ kind: 'page', id: saved.id, mode: 'view', revision: null }, true); }}/>;
  return <ArtifactPage key={route.id} id={route.id} mode={route.mode} revision={route.revision} spaces={spaces} theme={theme} go={go}
    backToList={() => go(list())} onLoaded={artifact => { if (artifact.space_id && artifact.space_id !== spaceId) onSpace(artifact.space_id); }}
    onDirty={onDirty} onChanged={onChanged}/>;
}
