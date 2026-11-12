import { useEffect, useId, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { FolderPlus, X } from 'lucide-react';
import './CreateSpaceDialog.css';

type SpaceDetails = { name: string; description: string };

type Props = {
  onCreate: (details: SpaceDetails) => Promise<void>;
  onClose: () => void;
};

export function CreateSpaceDialog({ onCreate, onClose }: Props) {
  const dialog = useRef<HTMLDialogElement>(null);
  const nameInput = useRef<HTMLInputElement>(null);
  const titleId = useId();
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [pending, setPending] = useState(false);
  const [error, setError] = useState('');

  useEffect(() => {
    const element = dialog.current;
    const opener = document.activeElement;
    element?.showModal();
    nameInput.current?.focus();
    return () => {
      element?.close();
      if (opener instanceof HTMLElement) opener.focus({ preventScroll: true });
    };
  }, []);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const trimmedName = name.trim();
    if (!trimmedName || pending) return;
    setPending(true);
    setError('');
    try {
      await onCreate({ name: trimmedName, description: description.trim() });
      onClose();
    } catch (error) {
      setError(error instanceof Error ? error.message : String(error));
      setPending(false);
    }
  };

  return <dialog ref={dialog} className="spaces-dialog create-space-dialog" aria-labelledby={titleId} onCancel={event => {
    event.preventDefault();
    if (!pending) onClose();
  }}>
    <header className="spaces-head">
      <h1 id={titleId}>Создать пространство</h1>
      <button type="button" className="icon-button" aria-label="Закрыть" onClick={onClose} disabled={pending}><X size={20}/></button>
    </header>
    <form className="spaces-edit" onSubmit={submit} aria-busy={pending}>
      <label className="field">
        <span>Название</span>
        <input ref={nameInput} required maxLength={200} value={name} onChange={event => setName(event.target.value)} disabled={pending}/>
      </label>
      <label className="field">
        <span>Описание (необязательно)</span>
        <textarea rows={3} value={description} onChange={event => setDescription(event.target.value)} disabled={pending}/>
      </label>
      {error && <p className="spaces-error" role="alert">{error}</p>}
      <div className="spaces-actions">
        <button type="button" className="button ghost" onClick={onClose} disabled={pending}>Отмена</button>
        <button type="submit" className="button primary" disabled={pending || !name.trim()}><FolderPlus size={15}/>{pending ? 'Создаём…' : 'Создать'}</button>
      </div>
    </form>
  </dialog>;
}
