import React from 'react';
import { Star } from 'lucide-react';
import type { Favorite, Space } from './api';

type Props = { favorites: Favorite[]; spaces: Space[]; currentSpace: string; selectedId: string; onOpen: (id: string) => void };

/** Favourites of all spaces, newest first. Hidden entirely while empty. */
export function FavoritesBlock({ favorites, spaces, currentSpace, selectedId, onOpen }: Props) {
  if (!favorites.length) return null;
  const names = new Map(spaces.map(s => [s.id, s.name]));
  return <section className="favorites-block" aria-label="Избранное">
    <div className="tree-caption favorites-caption"><span><Star size={11}/> ИЗБРАННОЕ</span><span>{favorites.length}</span></div>
    <div className="favorites-list">{favorites.map(f => <button key={f.id} className={`tree-item favorite-item ${selectedId === f.id ? 'selected' : ''}`} onClick={() => onOpen(f.id)} title={f.title}>
      <span className="tree-title">{f.title || 'Без названия'}</span>
      {f.space_id !== currentSpace && <span className="space-tag" title="Другое пространство">{names.get(f.space_id) || f.space_id}</span>}
    </button>)}</div>
  </section>;
}
