import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { AlertTriangle, ArrowLeft, FilePlus2, FileText, Folder, Image as ImageIcon, Pencil, Save, Trash2, Upload } from 'lucide-react';
import { client, errorStatus } from '../api';
import type { ArtifactDetail, Project, Space } from '../api';
import {
  buildTree, buildUpdate, checkNewPath, contentBytes, diffFiles, draftFromManifest, ENTRY_PATH, formatSize, isImagePath, isTextPath, LIMITS,
  validateDraft, type DraftFile, type TreeNode, type Visibility,
} from '../artifactsCore';
import { MarkdownEditor } from '../MarkdownEditor';
import { decodeServerFile, mimeForPath, readUserFile } from './fileio';
import { Modal } from './Modal';
import { TEMPLATES, type TemplateId } from './templates';

type Props = {
  /** Present when editing; absent when creating. */
  artifact?: ArtifactDetail;
  spaces: Space[];
  initialSpace: string;
  initialProject: string;
  theme: 'light' | 'dark';
  onDirty: (dirty: boolean) => void;
  onSaved: (artifact: ArtifactDetail) => void;
  onCancel: () => void;
  /** After a 409 the user chose to drop their draft and take the current server version. */
  onReload: () => void;
};

const fileKey = (file: DraftFile) => `${file.path}\n${file.encoding}\n${file.content}`;

export function ArtifactEditor({ artifact, spaces, initialSpace, initialProject, theme, onDirty, onSaved, onCancel, onReload }: Props) {
  const creating = !artifact;
  const [templateId, setTemplateId] = useState<TemplateId>('empty');
  const startFiles = useMemo(() => artifact ? draftFromManifest(artifact.manifest) : TEMPLATES[0].files(), [artifact]);
  const [files, setFiles] = useState<DraftFile[]>(startFiles);
  const [selected, setSelected] = useState(ENTRY_PATH);
  const [title, setTitle] = useState(artifact?.title || '');
  const [description, setDescription] = useState(artifact?.description || '');
  const [visibility, setVisibility] = useState<Visibility>(artifact?.visibility || 'inherit');
  const [spaceId, setSpaceId] = useState(initialSpace);
  const [projectId, setProjectId] = useState(initialProject);
  const [projects, setProjects] = useState<Project[]>([]);
  const [message, setMessage] = useState('');
  const [newName, setNewName] = useState('');
  const [fileError, setFileError] = useState('');
  const [loadError, setLoadError] = useState('');
  const [loadingPath, setLoadingPath] = useState('');
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState('');
  const [attempted, setAttempted] = useState(false);
  const [conflict, setConflict] = useState<{ revision: number } | null>(null);
  const [renaming, setRenaming] = useState<DraftFile | null>(null);
  const [renameValue, setRenameValue] = useState('');
  const [renameError, setRenameError] = useState('');
  const uploadRef = useRef<HTMLInputElement>(null);
  const templateTouched = useRef(false);

  // The project list follows the chosen space (creation only).
  useEffect(() => {
    if (!creating || !spaceId) { setProjects([]); return; }
    let live = true;
    client.projects(spaceId).then(list => { if (live) setProjects(list.filter(project => !project.archived_at)); }).catch(() => { if (live) setProjects([]); });
    return () => { live = false; };
  }, [creating, spaceId]);

  const current = files.find(file => file.path === selected) || null;

  // Text files that exist on the server are fetched when opened.
  useEffect(() => {
    if (!artifact || !current || current.content !== null || !isTextPath(current.path)) return;
    let live = true; const path = current.path;
    setLoadingPath(path); setLoadError('');
    client.artifactFile(artifact.id, path, artifact.revision).then(buffer => {
      if (!live) return;
      const loaded = decodeServerFile(path, buffer);
      setFiles(list => list.map(file => file.path === path && file.content === null ? { ...file, ...loaded } : file));
    }).catch(e => { if (live) setLoadError(`Не удалось загрузить ${path}: ${(e as Error).message}`); }).finally(() => { if (live) setLoadingPath(''); });
    return () => { live = false; };
  }, [artifact, current?.path, current?.content]);

  const meta = { title, description, visibility };
  const update = useMemo(() => artifact ? buildUpdate(artifact, meta, files, message) : null, [artifact, title, description, visibility, files, message]);
  const dirty = creating
    ? Boolean(title.trim() || description.trim() || templateTouched.current || visibility !== 'inherit' || files.length !== startFiles.length || files.some((file, i) => fileKey(file) !== fileKey(startFiles[i])))
    : Boolean(update?.changed);
  const dirtyRef = useRef(false); dirtyRef.current = dirty;
  useEffect(() => { onDirty(dirty); return () => onDirty(false); }, [dirty]);
  useEffect(() => {
    if (!dirty) return;
    const handler = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ''; };
    window.addEventListener('beforeunload', handler);
    return () => window.removeEventListener('beforeunload', handler);
  }, [dirty]);

  const problems = useMemo(() => validateDraft(files), [files]);
  const total = files.reduce((sum, file) => sum + file.size, 0);
  const tree = useMemo(() => buildTree(files), [files]);

  const setContent = (path: string, content: string) => setFiles(list => list.map(file => file.path === path ? { ...file, content, size: contentBytes(content, file.encoding) } : file));

  const chooseTemplate = (id: TemplateId) => {
    const hasWork = files.length !== 1 || files[0].path !== ENTRY_PATH || templateTouched.current;
    if (id === templateId) return;
    if (hasWork && !window.confirm('Заменить файлы выбранным шаблоном? Текущие файлы будут потеряны.')) return;
    const template = TEMPLATES.find(item => item.id === id)!;
    setTemplateId(id); setFiles(template.files()); setSelected(ENTRY_PATH); templateTouched.current = id !== 'empty';
  };

  const addFile = (event: React.FormEvent) => {
    event.preventDefault();
    const path = newName.trim();
    const problem = checkNewPath(path, files);
    if (problem) { setFileError(problem); return; }
    setFiles(list => [...list, { path, encoding: isTextPath(path) ? 'utf8' : 'base64', content: '', original: null, size: 0 }]);
    setSelected(path); setNewName(''); setFileError('');
    if (!isTextPath(path)) setFileError('Это изображение: пустой файл не имеет смысла, загрузите его кнопкой «Загрузить».');
  };

  const upload = async (list: FileList | null) => {
    if (!list?.length) return;
    const added: DraftFile[] = []; const notes: string[] = [];
    let known = [...files];
    for (const source of Array.from(list)) {
      try {
        const file = await readUserFile(source);
        const replace = known.some(item => item.path === file.path);
        const problem = checkNewPath(file.path, known, replace ? file.path : '');
        if (problem) { notes.push(`${source.name}: ${problem}`); continue; }
        if (file.size > LIMITS.maxFileBytes) { notes.push(`${source.name}: больше ${formatSize(LIMITS.maxFileBytes)}.`); continue; }
        known = [...known.filter(item => item.path !== file.path), file]; added.push(file);
      } catch (e) { notes.push(`${source.name}: ${(e as Error).message}`); }
    }
    if (added.length) {
      setFiles(prev => [...prev.filter(item => !added.some(file => file.path === item.path)), ...added]);
      setSelected(added[0].path);
    }
    setFileError(notes.join(' '));
    if (uploadRef.current) uploadRef.current.value = '';
  };

  const ensureLoaded = async (file: DraftFile): Promise<DraftFile> => {
    if (file.content !== null || !artifact) return file;
    const loaded = decodeServerFile(file.path, await client.artifactFile(artifact.id, file.path, artifact.revision));
    return { ...file, ...loaded };
  };
  const startRename = (file: DraftFile) => { setRenaming(file); setRenameValue(file.path); setRenameError(''); };
  const applyRename = async (event: React.FormEvent) => {
    event.preventDefault();
    if (!renaming) return;
    const path = renameValue.trim();
    if (path === renaming.path) { setRenaming(null); return; }
    const problem = checkNewPath(path, files, renaming.path);
    if (problem) { setRenameError(problem); return; }
    if (isTextPath(path) !== isTextPath(renaming.path)) { setRenameError('Расширение должно остаться того же вида (текст или изображение).'); return; }
    try {
      const loaded = await ensureLoaded(renaming);
      // Renaming = delete + create; the new file has no "original" so it is always written.
      setFiles(list => list.map(file => file.path === renaming.path ? { ...loaded, path, original: null } : file));
      if (selected === renaming.path) setSelected(path);
      setRenaming(null);
    } catch (e) { setRenameError(`Не удалось прочитать файл: ${(e as Error).message}`); }
  };
  const removeFile = (file: DraftFile) => {
    if (!window.confirm(`Удалить файл ${file.path}?`)) return;
    setFiles(list => list.filter(item => item.path !== file.path));
    if (selected === file.path) setSelected(ENTRY_PATH);
  };

  const save = async () => {
    setAttempted(true); setSaveError('');
    if (!title.trim() || problems.length) return;
    if (creating && !spaceId) { setSaveError('Выберите пространство.'); return; }
    if (artifact && update && !update.changed) { onCancel(); return; }
    setSaving(true);
    try {
      let saved: ArtifactDetail;
      if (artifact && update) saved = await client.updateArtifact(artifact.id, update.body);
      else {
        const complete = await Promise.all(files.map(ensureLoaded));
        saved = await client.createArtifact({
          space_id: spaceId, ...(projectId ? { project_id: projectId } : {}), title: title.trim(), description: description.trim(), visibility,
          files: complete.map(file => ({ path: file.path, content: file.content || '', encoding: file.encoding })), ...(message.trim() ? { message: message.trim() } : {}),
        });
      }
      dirtyRef.current = false; onDirty(false);
      onSaved(saved);
    } catch (e) {
      if (artifact && errorStatus(e) === 409) {
        let revision = artifact.revision;
        try { revision = (await client.artifact(artifact.id)).revision; } catch { /* the number is only informative */ }
        setConflict({ revision });
      } else setSaveError((e as Error).message);
    } finally { setSaving(false); }
  };

  const spaceOptions = spaces.filter(space => !space.archived_at);
  const renderNode = (node: TreeNode, depth: number): React.ReactNode => {
    if (node.type === 'dir') return <li key={`d:${node.path}`}><div className="art-tree-dir" style={{ paddingLeft: depth * 14 + 8 }}><Folder size={14} aria-hidden="true"/>{node.name}</div><ul>{node.children.map(child => renderNode(child, depth + 1))}</ul></li>;
    const file = files.find(item => item.path === node.path)!;
    const Icon = isImagePath(node.path) ? ImageIcon : FileText;
    return <li key={`f:${node.path}`} className={node.path === selected ? 'selected' : ''}>
      <button type="button" className="art-tree-file" style={{ paddingLeft: depth * 14 + 8 }} onClick={() => { setSelected(node.path); setFileError(''); }} aria-current={node.path === selected ? 'true' : undefined}><Icon size={14} aria-hidden="true"/><span>{node.name}</span><small>{formatSize(node.size)}</small></button>
      <button type="button" className="icon-button tiny" onClick={() => startRename(file)} title="Переименовать" aria-label={`Переименовать ${node.path}`}><Pencil size={13}/></button>
      <button type="button" className="icon-button tiny" onClick={() => removeFile(file)} title="Удалить файл" aria-label={`Удалить ${node.path}`}><Trash2 size={13}/></button>
    </li>;
  };

  return <section className="art-editor">
    <div className="art-top"><button type="button" className="button ghost" onClick={onCancel}><ArrowLeft size={16}/>{creating ? 'К списку' : 'К артефакту'}</button><span className="eyebrow">{creating ? 'НОВЫЙ АРТЕФАКТ' : `ПРАВКА · ВЕРСИЯ ${artifact!.revision}`}</span></div>
    <h1>{creating ? 'Создать артефакт' : 'Редактировать артефакт'}</h1>

    {conflict && <div className="art-alert error" role="alert">
      <AlertTriangle size={17} aria-hidden="true"/>
      <div><b>Артефакт изменён после начала правки.</b> Сейчас версия {conflict.revision}, вы редактировали версию {artifact!.revision}. Ваши правки не сохранены. Загрузите свежую версию (черновик будет потерян) или скопируйте нужное из редактора и вернитесь.
        <div className="art-actions left"><button type="button" className="button primary small" onClick={() => { dirtyRef.current = false; onDirty(false); onReload(); }}>Загрузить свежую версию</button><button type="button" className="button ghost small" onClick={() => setConflict(null)}>Остаться и скопировать</button></div></div>
    </div>}

    {creating && <fieldset className="art-templates"><legend>Шаблон</legend>
      {TEMPLATES.map(template => <label key={template.id} className={template.id === templateId ? 'selected' : ''}><input type="radio" name="art-template" checked={template.id === templateId} onChange={() => chooseTemplate(template.id)}/><b>{template.title}</b><small>{template.description}</small></label>)}
    </fieldset>}

    <div className="art-form">
      <label className="field wide"><span>Название <b>*</b></span><input maxLength={300} value={title} onChange={event => setTitle(event.target.value)} placeholder="Например, воронка продаж" aria-invalid={attempted && !title.trim()}/>{attempted && !title.trim() && <small className="art-error-text">Введите название.</small>}</label>
      <label className="field wide"><span>Описание</span><input value={description} onChange={event => setDescription(event.target.value)} placeholder="Одно предложение: что показывает артефакт"/></label>
      {creating && <>
        <label className="field"><span>Пространство</span><select value={spaceId} onChange={event => { setSpaceId(event.target.value); setProjectId(''); }}>{!spaceOptions.some(space => space.id === spaceId) && <option value={spaceId}>Выберите…</option>}{spaceOptions.map(space => <option key={space.id} value={space.id}>{space.name}</option>)}</select></label>
        <label className="field"><span>Проект (необязательно)</span><select value={projectId} onChange={event => setProjectId(event.target.value)}><option value="">Без проекта</option>{projects.map(project => <option key={project.id} value={project.id}>{project.name}</option>)}</select></label>
      </>}
      <label className="field"><span>Видимость</span><select value={visibility} disabled={!creating && !artifact!.can_manage} onChange={event => setVisibility(event.target.value as Visibility)}><option value="inherit">Видно участникам пространства</option><option value="private">Только мне</option></select></label>
      <label className="field"><span>Комментарий к версии</span><input value={message} onChange={event => setMessage(event.target.value)} placeholder="Что изменилось (необязательно)"/></label>
    </div>

    <div className="art-files">
      <aside className="art-file-list" aria-label="Файлы артефакта">
        <div className="art-file-head"><b>Файлы</b><small>{files.length} из {LIMITS.maxFiles} · {formatSize(total)} из {formatSize(LIMITS.maxRevisionBytes)}</small></div>
        <ul className="art-tree">{tree.map(node => renderNode(node, 0))}</ul>
        <form className="art-add-file" onSubmit={addFile}>
          <input value={newName} onChange={event => { setNewName(event.target.value); setFileError(''); }} placeholder="папка/имя.js" aria-label="Имя нового файла"/>
          <button type="submit" className="button small" title="Добавить пустой файл"><FilePlus2 size={14}/>Добавить</button>
        </form>
        <input ref={uploadRef} type="file" multiple hidden accept=".html,.css,.js,.mjs,.json,.csv,.txt,.md,.svg,.png,.jpg,.jpeg,.gif,.webp" onChange={event => void upload(event.target.files)}/>
        <button type="button" className="button small" onClick={() => uploadRef.current?.click()}><Upload size={14}/>Загрузить файлы</button>
        {fileError && <p className="art-error-text" role="alert">{fileError}</p>}
      </aside>
      <div className="art-file-pane">
        {!current ? <p className="art-muted">Выберите файл слева.</p> : <>
          <div className="art-file-title"><code>{current.path}</code><small>{formatSize(current.size)}</small></div>
          {loadError && <p className="art-error-text" role="alert">{loadError}</p>}
          {isTextPath(current.path) && current.encoding === 'utf8'
            ? (current.content === null ? <p className="art-muted" role="status">{loadingPath === current.path ? 'Загружаем файл…' : 'Файл не загружен.'}</p>
              : <div className={`art-code${current.path.toLowerCase().endsWith('.md') ? ' md' : ''}`}><MarkdownEditor key={current.path} value={current.content} onChange={value => setContent(current.path, value)} theme={theme} ariaLabel={`Содержимое ${current.path}`}/></div>)
            : <div className="art-binary">
              {current.content !== null && isImagePath(current.path) ? <img src={`data:${mimeForPath(current.path)};base64,${current.content}`} alt={current.path}/> : <p className="art-muted">Это двоичный файл ({formatSize(current.size)}); его нельзя править как текст. Чтобы заменить, загрузите файл с тем же именем.</p>}
            </div>}
        </>}
      </div>
    </div>

    {attempted && problems.length > 0 && <div className="art-alert error" role="alert"><AlertTriangle size={16} aria-hidden="true"/><ul>{problems.map((problem, i) => <li key={i}>{problem}</li>)}</ul></div>}
    {!attempted && problems.length > 0 && <div className="art-alert warn" role="status"><ul>{problems.map((problem, i) => <li key={i}>{problem}</li>)}</ul></div>}
    {saveError && <div className="art-alert error" role="alert">{saveError}</div>}
    <div className="art-actions">
      <button type="button" className="button ghost" onClick={onCancel} disabled={saving}>Отмена</button>
      <button type="button" className="button primary" onClick={() => void save()} disabled={saving}><Save size={15}/>{saving ? 'Сохраняем…' : creating ? 'Создать артефакт' : 'Сохранить'}</button>
    </div>
    {!creating && update && !update.changed && <p className="art-muted">Изменений пока нет.</p>}
    {!creating && update?.changed && <p className="art-muted">Будет отправлено: {diffFiles(artifact!.manifest, files).length} файл(ов) с изменениями{update.body.title !== undefined || update.body.description !== undefined || update.body.visibility !== undefined ? ' и сведения об артефакте' : ''}.</p>}

    {renaming && <Modal title="Переименовать файл" onClose={() => setRenaming(null)}>
      <form onSubmit={e => void applyRename(e)}>
        <label className="field"><span>Новый путь (папки через «/»)</span><input data-autofocus value={renameValue} onChange={event => { setRenameValue(event.target.value); setRenameError(''); }} aria-invalid={Boolean(renameError)}/></label>
        {renameError && <p className="art-error-text" role="alert">{renameError}</p>}
        <p className="art-muted">Переименование на сервере выполняется как удаление и создание файла.</p>
        <div className="art-actions"><button type="button" className="button ghost" onClick={() => setRenaming(null)}>Отмена</button><button type="submit" className="button primary">Переименовать</button></div>
      </form>
    </Modal>}
  </section>;
}
