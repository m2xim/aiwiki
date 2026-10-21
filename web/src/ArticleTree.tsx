import React from 'react';
import { ChevronDown, ChevronRight, Pin } from 'lucide-react';
import type { Article } from './api';

type Props = {
  tree: Map<string, Article[]>; openNodes: Record<string, boolean>; selectedId: string;
  onToggle: (id: string, expanded: boolean) => void; onSelect: (id: string) => void;
};

/** Memoised article tree: typing in a form field elsewhere does not re-render it. */
export const ArticleTree = React.memo(function ArticleTree({ tree, openNodes, selectedId, onToggle, onSelect }: Props) {
  const render = (parent: string, depth: number): React.ReactNode => (tree.get(parent) || []).map(a => {
    const hasChildren = (tree.get(a.id)?.length ?? 0) > 0;
    const expanded = openNodes[a.id] !== false;
    const title = a.title || 'Без названия';
    const selected = selectedId === a.id;
    return <React.Fragment key={a.id}><div className="tree-row" style={{ paddingLeft: 12 + depth * 15 }}>
      {hasChildren && <button type="button" className="tree-toggle" aria-expanded={expanded} aria-label={`${expanded ? 'Свернуть' : 'Развернуть'} ${title}`} title={`${expanded ? 'Свернуть' : 'Развернуть'} ${title}`} onClick={() => onToggle(a.id, expanded)}>{expanded ? <ChevronDown size={14}/> : <ChevronRight size={14}/>}</button>}
      <button className={`tree-item ${selected ? 'selected' : ''}`} aria-current={selected ? 'page' : undefined} onClick={() => onSelect(a.id)} title={title}>{a.pinned && <Pin size={12} className="pin-mini"/>}<span className="tree-title">{title}</span>{a.kind === 'instruction' && <span className="tree-badge">i</span>}</button>
    </div>{expanded && render(a.id, depth + 1)}</React.Fragment>;
  });
  return <>{render('', 0)}</>;
});
