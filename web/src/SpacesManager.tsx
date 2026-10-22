import { useEffect, useRef, useState } from 'react';
import { Archive, ArrowLeft, Check, FolderPlus, Pencil, X } from 'lucide-react';
import { client, Space } from './api';
import { SpaceStorageInfo } from './SpaceStorageInfo';

export function SpacesManager({ spaces, draftSpaceIds, corporate = false, admin = false, initialEditId, initialTab = 'active', onChange, onClose }: { spaces: Space[]; draftSpaceIds: string[]; corporate?: boolean; admin?: boolean; initialEditId?: string; initialTab?: 'active' | 'archive'; onChange: (space: Space) => void; onClose: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const initial = initialEditId ? spaces.find(space => space.id === initialEditId) : undefined;
  const direct = Boolean(initial); // opened straight into the edit form: leaving the form closes the dialog
  const [tab, setTab] = useState<'active' | 'archive'>(initial ? (initial.archived_at ? 'archive' : 'active') : initialTab);
  const [editing, setEditing] = useState<string | null>(initial ? initial.id : null);
  const [name, setName] = useState(initial?.name || '');
  const [description, setDescription] = useState(initial?.description || '');
  const [pending, setPending] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => {
    const element = dialog.current;
    const pageY = window.scrollY;
    const root = document.documentElement, body = document.body;
    const savedRoot = { overflow: root.style.overflow, scrollBehavior: root.style.scrollBehavior };
    const savedBody = { position: body.style.position, top: body.style.top, left: body.style.left, right: body.style.right, width: body.style.width, overflow: body.style.overflow };
    const scrollers = Array.from(document.querySelectorAll<HTMLElement>('.workspace, .workspace *')).filter(node => node.scrollHeight > node.clientHeight && getComputedStyle(node).overflowY !== 'visible').map(node => ({ node, overflow: node.style.overflow, overflowY: node.style.overflowY }));
    root.style.overflow = 'hidden'; root.style.scrollBehavior = 'auto';
    body.style.position = 'fixed'; body.style.top = `${-pageY}px`; body.style.left = '0'; body.style.right = '0'; body.style.width = '100%'; body.style.overflow = 'hidden';
    scrollers.forEach(({ node }) => { node.style.overflow = 'hidden'; node.style.overflowY = 'hidden'; });
    element?.showModal();
    return () => {
      element?.close();
      root.style.overflow = savedRoot.overflow; root.style.scrollBehavior = savedRoot.scrollBehavior;
      Object.assign(body.style, savedBody);
      scrollers.forEach(({ node, overflow, overflowY }) => { node.style.overflow = overflow; node.style.overflowY = overflowY; });
      window.scrollTo(0, pageY);
    };
  }, []);
  const edit = (space?: Space) => { setEditing(space?.id || ''); setName(space?.name || ''); setDescription(space?.description || ''); setError(''); };
  const save = async () => {
    if (!name.trim() || pending) return;
    setPending(true); setError('');
    try { const space = editing ? await client.updateSpace(editing, { name: name.trim(), description: description.trim() }) : await client.createSpace(name.trim(), description.trim()); onChange(space); if (direct) onClose(); else setEditing(null); }
    catch (e) { setError((e as Error).message); } finally { setPending(false); }
  };
  const archive = async (space: Space) => {
    setPending(true); setError('');
    try { const updated = await client.updateSpace(space.id, { archived: !space.archived_at }); onChange(updated); if (direct) onClose(); }
    catch (e) { setError((e as Error).message); } finally { setPending(false); }
  };
  const editingSpace = editing ? spaces.find(space => space.id === editing) : undefined;
  const listed = spaces.filter(space => Boolean(space.archived_at) === (tab === 'archive'));
  return <dialog ref={dialog} className="spaces-dialog" aria-labelledby="spaces-title" onCancel={e => { e.preventDefault(); if (!pending) onClose(); }}>
    <header className="spaces-head"><div><div className="eyebrow">БАЗА ЗНАНИЙ</div><h1 id="spaces-title">Управление пространствами</h1></div><button className="icon-button" aria-label="Закрыть управление пространствами" disabled={pending} onClick={onClose}><X size={20}/></button></header>
    <p className="spaces-intro">Архив скрывает пространство из списка активных. Все материалы сохраняются, пространство можно восстановить.</p>
    {draftSpaceIds.length > 0 && <p className="spaces-draft-note">Черновик сохранён в открытом редакторе. Чтобы архивировать его пространство, сначала завершите редактирование.</p>}
    {error && <p className="spaces-error" role="alert">{error}</p>}
    {editing !== null ? <form className="spaces-edit" onSubmit={e => { e.preventDefault(); void save(); }}><h2>{editing ? 'Изменить пространство' : 'Новое пространство'}</h2><label className="field"><span>Название</span><input autoFocus required maxLength={200} value={name} onChange={e=>setName(e.target.value)} disabled={pending}/></label><label className="field"><span>Описание</span><textarea rows={3} value={description} onChange={e=>setDescription(e.target.value)} disabled={pending}/></label>{corporate && editing && <SpaceStorageInfo key={editing} spaceId={editing} admin={admin}/>}<div className="spaces-actions">{direct && editingSpace && <button type="button" className="button ghost spaces-archive-toggle" disabled={pending || (!editingSpace.archived_at && draftSpaceIds.includes(editingSpace.id))} onClick={()=>void archive(editingSpace)}>{editingSpace.archived_at?<ArrowLeft size={15}/>:<Archive size={15}/>}{editingSpace.archived_at?'Восстановить':'В архив'}</button>}<button type="button" className="button ghost" disabled={pending} onClick={()=>direct ? onClose() : setEditing(null)}>Отмена</button><button className="button primary" disabled={pending || !name.trim()}><Check size={15}/>{pending ? 'Сохраняем…' : 'Сохранить'}</button></div></form> : <>
      <div className="spaces-toolbar"><div className="spaces-tabs" role="group" aria-label="Состояние пространств"><button className={`button ${tab==='active'?'selected-tab':''}`} disabled={pending} onClick={()=>setTab('active')}>Активные ({spaces.filter(s=>!s.archived_at).length})</button><button className={`button ${tab==='archive'?'selected-tab':''}`} disabled={pending} onClick={()=>setTab('archive')}>Архив ({spaces.filter(s=>s.archived_at).length})</button></div><button className="button primary" disabled={pending} onClick={()=>edit()}><FolderPlus size={15}/>Создать</button></div>
      <div className="spaces-list" aria-busy={pending}>{listed.map(space=><section className="space-card" key={space.id}><div><h2>{space.name}</h2>{space.description&&<p>{space.description}</p>}{space.archived_at&&<small>В архиве</small>}</div><div className="spaces-actions"><button className="button ghost small" disabled={pending} onClick={()=>edit(space)}><Pencil size={14}/>Изменить</button><button className="button ghost small" disabled={pending || (!space.archived_at && draftSpaceIds.includes(space.id))} onClick={()=>void archive(space)}>{space.archived_at?<ArrowLeft size={14}/>:<Archive size={14}/>} {space.archived_at?'Восстановить':'В архив'}</button></div></section>)}{!listed.length&&<div className="panel-empty">{tab==='archive'?'Архив пуст.':'Нет активных пространств. Создайте новое или восстановите пространство из архива.'}</div>}</div>
    </>}
  </dialog>;
}
