import React, { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { Activity, Archive, ArrowDownToLine, ArrowLeft, BookOpen, Bot, Check, ChevronDown, ChevronRight, CircleHelp, Clock3, Code2, Columns2, FileImage, FolderPlus, FolderTree, History, ImagePlus, Link2, List, ListFilter, MessageCircle, Moon, Pencil, Pin, Plus, Search, Settings2, ShieldCheck, Sparkles, Star, Sun, Tag, Trash2, Upload, X } from 'lucide-react';
import { Article, Attachment, AuthSession, Bootstrap, Favorite, Project, Revision, SearchHit, SearchResponse, Space, client, setAuthExpiredHandler, setCorporateSession, setCsrfToken } from './api';
import { headings, Markdown, scrollToFirstHighlight, smoothBehavior } from './markdown';
import { Toc } from './Toc';
import { SpacesManager } from './SpacesManager';
import { HistoryPage } from './HistoryPage';
import { AuditPage } from './AuditPage';
import { ArticleComments, ArticleCommentsHandle, commentsLabel } from './ArticleComments';
import { Breadcrumbs } from './Breadcrumbs';
import { ArticleTree } from './ArticleTree';
import { FavoritesBlock } from './Favorites';
import { SearchResultsList } from './SearchResults';
import { lastUrlRef, SIDEBAR_MAX, SIDEBAR_MIN, UNSAVED_MESSAGE, articleUrl, currentUrl, isTypingTarget, readRoute, spaceUrl, useSidebarWidth } from './nav';
import type { MarkdownEditorHandle } from './MarkdownEditor';
import { ParentPicker } from './ParentPicker';
import { EditorConflict } from './EditorConflict';
import { clearCorporateDrafts, DraftFields, setDraftIdentity, StoredDraft, draftKey, formatDraftTime, loadDraft, loadParkedList, popParked, pushParked, removeDraft, sameFields, saveDraft } from './draftStore';
import { AdminPanel, adminUrl, readAdminTab } from './AdminPanel';
import { ArticleExportMenu } from './ArticleExportMenu';
const ImportMarkdownDialog = lazy(() => import('./ImportMarkdown').then(m => ({ default: m.ImportMarkdownDialog })));
import './auth-gate.css';
import './styles.css';
import './editor-wave2.css';

const MarkdownEditor = lazy(() => import('./MarkdownEditor').then(module => ({ default: module.MarkdownEditor })));

const kinds = ['article', 'instruction', 'research', 'plan', 'source'];
const statuses = ['draft', 'active', 'verified', 'archived'];
const kindNames: Record<string, string> = { article: 'Статья', instruction: 'Инструкция', research: 'Исследование', plan: 'План', source: 'Источник' };
const statusNames: Record<string, string> = { draft: 'Черновик', active: 'Актуально', verified: 'Проверено', archived: 'В архиве' };
const prettyDate = (date: string) => date ? new Intl.DateTimeFormat('ru-RU', { day: 'numeric', month: 'short', year: 'numeric' }).format(new Date(date)) : '';
const bytes = (n: number) => n < 1024 ? `${n} Б` : n < 1024 * 1024 ? `${(n / 1024).toFixed(0)} КБ` : `${(n / 1024 / 1024).toFixed(1)} МБ`;
const authorKey = 'aiwiki.author';
const readAuthor = () => { try { return localStorage.getItem(authorKey) || ''; } catch { return ''; } };
const storeAuthor = (value: string) => { try { localStorage.setItem(authorKey, value); } catch { /* Author remains available for this session. */ } };
const isNarrow = () => window.matchMedia?.('(max-width: 850px)').matches ?? false;

/** Fields whose change counts as «unsaved» in the editor. */
type DraftSnapshot = DraftFields;
const snapshotOf = (d: Article, tags: string, properties: string): DraftSnapshot => ({ title: d.title, summary: d.summary, body: d.body, kind: d.kind, status: d.status, pinned: d.pinned, space_id: d.space_id, project_id: d.project_id || '', parent_id: d.parent_id, tags, properties });
const sameSnapshot = (a: DraftSnapshot, b: DraftSnapshot) => (Object.keys(a) as (keyof DraftSnapshot)[]).every(key => a[key] === b[key]);

/** «key=value» lines; a non-empty line without a key (no «=», or nothing before it) cannot be stored and is reported back. */
const parseProperties = (text: string) => {
  const properties: Record<string, string> = {}; const invalid: string[] = [];
  text.split('\n').forEach(line => { if (!line.trim()) return; const i = line.indexOf('='); const key = i > 0 ? line.slice(0, i).trim() : ''; if (key) properties[key] = line.slice(i + 1).trim(); else invalid.push(line.trim()); });
  return { properties, invalid };
};
const WIDE_QUERY = '(min-width: 1300px)';
function useWide() {
  const [wide, setWide] = useState(() => window.matchMedia?.(WIDE_QUERY).matches ?? false);
  useEffect(() => { const query = window.matchMedia?.(WIDE_QUERY); if (!query) return; const on = () => setWide(query.matches); on(); query.addEventListener('change', on); window.addEventListener('resize', on); return () => { query.removeEventListener('change', on); window.removeEventListener('resize', on); }; }, []);
  return wide;
}
function useDebounced<T>(value: T, ms: number) {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => { const timer = window.setTimeout(() => setDebounced(value), ms); return () => window.clearTimeout(timer); }, [value, ms]);
  return debounced;
}

const initialRoute = readRoute();

function App({ session, onLogout, logoutError }: { session: AuthSession; onLogout: () => void; logoutError: string }) {
  const [theme, setTheme] = useState<'light' | 'dark'>(() => document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light');
  const [spaceManager, setSpaceManager] = useState(false);
  // With ?article=ID the space is unknown until the article arrives, so nothing is loaded for a guessed space first.
  const [booting, setBooting] = useState(Boolean(initialRoute.article));
  const [spaces, setSpaces] = useState<Space[]>([]); const [spaceId, setSpaceId] = useState(initialRoute.article ? '' : initialRoute.space || (session.mode === 'corporate' ? '' : 'general')); const [tagOptions, setTagOptions] = useState<string[]>([]);
  const [articles, setArticles] = useState<Article[]>([]); const [parentOptions, setParentOptions] = useState<Article[]>([]); const [selected, setSelected] = useState<Article | null>(null);
  const [articleLoading, setArticleLoading] = useState(Boolean(initialRoute.article)); const [loadingId, setLoadingId] = useState(initialRoute.article);
  const [bootstrap, setBootstrap] = useState<Bootstrap | null>(null); const [busy, setBusy] = useState(true); const [error, setError] = useState('');
  const [query, setQuery] = useState(''); const [tagFilter, setTagFilter] = useState(''); const [propertyFilter, setPropertyFilter] = useState(''); const [kindFilter, setKindFilter] = useState(''); const [pinnedOnly, setPinnedOnly] = useState(false); const [searchAll, setSearchAll] = useState(false);
  const [searchResponse, setSearchResponse] = useState<SearchResponse | null>(null); const [searchItems, setSearchItems] = useState<SearchHit[]>([]); const [searchLoading, setSearchLoading] = useState(false); const [searchMoreBusy, setSearchMoreBusy] = useState(false); const [searchNonce, setSearchNonce] = useState(0);
  const [favorites, setFavorites] = useState<Favorite[]>([]); const [favPending, setFavPending] = useState<string[]>([]);
  const [editor, setEditor] = useState(false); const [preview, setPreview] = useState(false); const [draft, setDraft] = useState<Article | null>(null); const [saving, setSaving] = useState(false); const [tagsText, setTagsText] = useState(''); const [propertiesText, setPropertiesText] = useState('');
  const [draftBase, setDraftBase] = useState(0); const [conflict, setConflict] = useState<Article | null>(null); const [offer, setOffer] = useState<StoredDraft | null>(null); const [parkedList, setParkedList] = useState<StoredDraft[]>([]); const parked = parkedList[0] ?? null; const offerRef = useRef<StoredDraft | null>(null); const lastSkipRef = useRef(false); const pendingScrollRef = useRef('');
  offerRef.current = offer;
  const [split, setSplit] = useState(false); const wide = useWide(); const splitActive = wide && split; const [propsAttempt, setPropsAttempt] = useState(false);
  const draftKeyRef = useRef(''); const latestDraftRef = useRef<{ key: string; fields: DraftFields; base: number } | null>(null); const conflictRef = useRef<HTMLDivElement>(null);
  const [author, setAuthor] = useState(readAuthor); const [panel, setPanel] = useState<'files' | 'ai' | 'toc' | null>(null);
  // Open comments per heading anchor (TOC badges) and search terms for the article opened from search results.
  const [anchorCounts, setAnchorCounts] = useState<Record<string, number>>({});
  const [searchHighlight, setSearchHighlight] = useState<{ id: string; terms: string[] }>({ id: '', terms: [] });
  const [revisions, setRevisions] = useState<Revision[]>([]); const [commentStats, setCommentStats] = useState({ open: 0, total: 0 }); const [attachments, setAttachments] = useState<Attachment[]>([]); const [links, setLinks] = useState<{ outgoing: Article[]; incoming: Article[] }>({ outgoing: [], incoming: [] }); const [pendingDelete, setPendingDelete] = useState<Attachment | null>(null); const [deletingAttachmentId, setDeletingAttachmentId] = useState(''); const [pendingDeleteArticle, setPendingDeleteArticle] = useState<Article | null>(null); const [deletingArticle, setDeletingArticle] = useState(false); const [undeletingArticle, setUndeletingArticle] = useState(false);
  const [historyView, setHistoryView] = useState(initialRoute.view === 'history' && Boolean(initialRoute.article)); const [historyArticleId, setHistoryArticleId] = useState(''); const [historyLoading, setHistoryLoading] = useState(false); const [historyError, setHistoryError] = useState(''); const [historyReload, setHistoryReload] = useState(0); const [restoring, setRestoring] = useState(false); const historyEntry = useRef(false);
  const [auditView, setAuditView] = useState(initialRoute.view === 'audit' && !initialRoute.article);
  const [spaceForm, setSpaceForm] = useState(false); const [spaceName, setSpaceName] = useState(''); const [spaceDescription, setSpaceDescription] = useState(''); const [notice, setNotice] = useState(''); const [pendingAnchor, setPendingAnchor] = useState('');
  const [adminOpen, setAdminOpen] = useState(session.mode === 'corporate' && initialRoute.view === 'admin');
  const [adminTab, setAdminTab] = useState(readAdminTab);
  const adminReturnUrl = useRef<string>(history.state?.aiwikiAdminReturn || spaceUrl(initialRoute.space));
  const [projects, setProjects] = useState<Project[]>([]); const [importOpen, setImportOpen] = useState(false);
  const [openNodes, setOpenNodes] = useState<Record<string, boolean>>({}); const [mobileNav, setMobileNav] = useState(false);
  const uploadRef = useRef<HTMLInputElement>(null); const bodyRef = useRef<MarkdownEditorHandle>(null); const commentsRef = useRef<ArticleCommentsHandle>(null); const deleteDialogRef = useRef<HTMLDivElement>(null); const deleteCancelRef = useRef<HTMLButtonElement>(null); const previousFocusRef = useRef<HTMLElement | null>(null); const searchInputRef = useRef<HTMLInputElement>(null);
  const articleRequest = useRef(0); const indexRequest = useRef(0); const searchRequest = useRef(0); const parentRequest = useRef(0); const tagRequest = useRef(0); const attachmentDeleteRequest = useRef(0);
  const selectedRef = useRef<Article | null>(null); const selectedIdRef = useRef(''); const spaceIdRef = useRef(spaceId); const editorOpenRef = useRef(false); const searchAllRef = useRef(searchAll); const baselineRef = useRef<DraftSnapshot | null>(null); const dirtyRef = useRef(false); const modalOpenRef = useRef(false); const overlayRef = useRef({ nav: false, panel: false });
  selectedRef.current = selected; selectedIdRef.current = selected?.id || ''; spaceIdRef.current = spaceId; editorOpenRef.current = editor; searchAllRef.current = searchAll;
  modalOpenRef.current = Boolean(pendingDelete || pendingDeleteArticle || spaceManager || adminOpen); overlayRef.current = { nav: mobileNav, panel: panel !== null };
  const sidebar = useSidebarWidth(!adminOpen);
  useEffect(() => { if (session.mode === 'corporate') { setAuthor(session.user.name); void client.projects().then(setProjects).catch(() => setProjects([])); } }, [session]);

  // ---- URL helpers: every URL write goes through here so the last known URL can be restored when a Back step is refused.
  const urlPush = useCallback((url: string) => { if (url === currentUrl()) { lastUrlRef.current = url; return; } history.pushState(null, '', url); lastUrlRef.current = url; }, []);
  const urlReplace = useCallback((url: string) => { history.replaceState(history.state, '', url); lastUrlRef.current = url; }, []);

  // ---- Unsaved-changes guard.
  const isDirty = Boolean(editor && draft && baselineRef.current && !sameSnapshot(baselineRef.current, snapshotOf(draft, tagsText, propertiesText)));
  dirtyRef.current = isDirty;
  const confirmLeave = useCallback(() => !dirtyRef.current || window.confirm(UNSAVED_MESSAGE), []);
  useEffect(() => {
    if (!isDirty) return;
    const onBeforeUnload = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ''; };
    window.addEventListener('beforeunload', onBeforeUnload);
    return () => window.removeEventListener('beforeunload', onBeforeUnload);
  }, [isDirty]);

  // ---- Local autosave of the draft (debounced); the stored copy is dropped only after a save or an explicit, confirmed cancel.
  useEffect(() => {
    if (!editor || !draft) { return; }
    const key = draftKeyRef.current;
    if (offer) {
      latestDraftRef.current = null;
      // The offered draft is still undecided. The first edit moves it to the parked slot so that both versions survive; autosave then continues.
      if (isDirty) { pushParked(key, offer); setParkedList(loadParkedList(key)); setOffer(null); }
      return;
    }
    if (!isDirty) { latestDraftRef.current = null; const timer = window.setTimeout(() => removeDraft(key), 1000); return () => window.clearTimeout(timer); }
    latestDraftRef.current = { key, fields: snapshotOf(draft, tagsText, propertiesText), base: draftBase };
    const timer = window.setTimeout(() => { const latest = latestDraftRef.current; if (latest) saveDraft(latest.key, latest.fields, latest.base); }, 1000);
    return () => window.clearTimeout(timer);
  }, [editor, draft, tagsText, propertiesText, isDirty, offer, draftBase]);
  useEffect(() => {
    if (!editor) return;
    const flush = () => { const latest = latestDraftRef.current; if (latest) saveDraft(latest.key, latest.fields, latest.base); };
    window.addEventListener('beforeunload', flush); window.addEventListener('pagehide', flush);
    return () => { window.removeEventListener('beforeunload', flush); window.removeEventListener('pagehide', flush); flush(); };
  }, [editor]);
  useEffect(() => { if (conflict) conflictRef.current?.scrollIntoView({ block: 'nearest', behavior: smoothBehavior() }); }, [conflict]);
  const mineFields = useMemo(() => draft && conflict ? snapshotOf(draft, tagsText, propertiesText) : null, [draft, tagsText, propertiesText, conflict]);
  const previewBody = useDebounced(splitActive && draft ? draft.body : '', 300);

  const resetFilters = useCallback(() => { setQuery(''); setTagFilter(''); setPropertyFilter(''); setKindFilter(''); setPinnedOnly(false); }, []);

  const loadIndex = useCallback(async (currentSpace = spaceId) => {
    const request = ++indexRequest.current; setBusy(true); setError('');
    try {
      const s = await client.spaces(true);
      if (request !== indexRequest.current) return;
      setSpaces(s);
      const available = s.find(item => item.id === currentSpace);
      const directArticle = new URLSearchParams(location.search).has('article');
      if (!available || (available.archived_at && !directArticle)) {
        const next = s.find(item => !item.archived_at)?.id || '';
        setArticles([]); setParentOptions([]); setBootstrap(null);
        if (currentSpace !== next) setSpaceId(next);
        return;
      }
      const boot = await client.bootstrap(currentSpace);
      const items: Article[] = []; const pageSize = 200;
      for (let offset = 0; ; offset += pageSize) {
        const page = await client.articles({ space_id: currentSpace, limit: String(pageSize), offset: String(offset) });
        if (request !== indexRequest.current) return;
        items.push(...page); if (page.length < pageSize) break;
      }
      if (request !== indexRequest.current) return;
      setSpaces(s); setArticles(items); setParentOptions(items); setBootstrap(boot);
    } catch (e) { if (request === indexRequest.current) setError((e as Error).message); } finally { if (request === indexRequest.current) setBusy(false); }
  }, [spaceId]);
  const loadTags = useCallback(async (currentSpace = spaceId) => {
    const request = ++tagRequest.current; if (!currentSpace) { setTagOptions([]); return; }
    try { const result = await client.tags(currentSpace); if (request === tagRequest.current && currentSpace === spaceIdRef.current) setTagOptions(result); }
    catch (e) { if (request === tagRequest.current && currentSpace === spaceIdRef.current) setError((e as Error).message); }
  }, [spaceId]);
  const loadFavorites = useCallback(async () => {
    try { setFavorites(await client.favorites()); } catch { /* favourites are optional: the sidebar block simply stays hidden */ }
  }, []);

  /** Loads an article and (optionally) writes its URL. The space follows the article. */
  const openArticle = useCallback(async (id: string, opts: { anchor?: string; mode?: 'push' | 'replace' | 'none'; view?: string; boot?: boolean; entry?: boolean } = {}) => {
    const { anchor = '', mode = 'push', view = '' } = opts;
    attachmentDeleteRequest.current++; setPendingDelete(null); setDeletingAttachmentId('');
    const request = ++articleRequest.current;
    setError(''); setArticleLoading(true); setLoadingId(id);
    try {
      const article = await client.article(id);
      if (request !== articleRequest.current) return;
      setSelected(article);
      if (article.space_id !== spaceIdRef.current) { if (!searchAllRef.current) resetFilters(); setSpaceId(article.space_id); }
      setPendingAnchor(anchor); setBooting(false); setArticleLoading(false);
      if (view === 'history') { setAuditView(false); setHistoryView(true); if (opts.entry) historyEntry.current = true; }
      const url = articleUrl(id, { view, anchor });
      if (mode === 'push') urlPush(url); else if (mode === 'replace') urlReplace(url);
    } catch (e) {
      if (request !== articleRequest.current) return;
      setError((e as Error).message); setArticleLoading(false);
      if (view === 'history') setHistoryView(false);
      if (opts.boot || spaceIdRef.current === '') { setBooting(false); setSpaceId(initialRoute.space || (session.mode === 'corporate' ? '' : 'general')); }
    }
  }, [resetFilters, urlPush, urlReplace, session]);

  useEffect(() => {
    if (booting) return;
    void loadIndex(spaceId); void loadTags(spaceId);
  }, [spaceId, booting]);
  useEffect(() => {
    void loadFavorites();
    const route = readRoute();
    if (!route.article) return;
    // Load the spaces list in parallel with the article so the picker is ready as soon as the page is.
    client.spaces(true).then(setSpaces).catch(() => { /* loadIndex reports problems later */ });
    void openArticle(route.article, { anchor: route.anchor, mode: 'none', view: route.view === 'history' ? 'history' : '', boot: true });
  }, []);

  // Keep ?space=ID in the address bar while no article is open (also normalises a bare "/" and a fallback space).
  useEffect(() => {
    if (booting || articleLoading || selected || !spaceId) return;
    const route = readRoute();
    if (route.view === 'admin' || route.article || route.space === spaceId) return;
    urlReplace(spaceUrl(spaceId, route.view === 'audit' ? 'audit' : ''));
  }, [booting, articleLoading, selected, spaceId, auditView]);

  // Back / Forward.
  const popRef = useRef<() => void>(() => undefined);
  popRef.current = () => {
    if (!confirmLeave()) { history.pushState(history.state, '', lastUrlRef.current); return; }
    const route = readRoute();
    const previousPath = lastUrlRef.current.split('#')[0];
    lastUrlRef.current = currentUrl();
    if (route.view === 'admin' && session.mode === 'corporate') {
      if (!adminOpen) adminReturnUrl.current = history.state?.aiwikiAdminReturn || previousPath;
      articleRequest.current++; setArticleLoading(false);
      leaveEditing(); setPanel(null); setMobileNav(false);
      setAdminTab(readAdminTab()); setAdminOpen(true); return;
    }
    setAdminOpen(false);
    if (location.pathname + location.search === previousPath && selectedRef.current && route.article === selectedRef.current.id) { setPendingAnchor(route.anchor); return; } // hash-only change
    setEditor(false); setDraft(null); setPanel(null); setMobileNav(false); historyEntry.current = false;
    attachmentDeleteRequest.current++; setPendingDelete(null); setDeletingAttachmentId('');
    if (route.article) {
      setAuditView(false); setHistoryView(route.view === 'history');
      if (route.article === selectedRef.current?.id) { articleRequest.current++; setArticleLoading(false); setPendingAnchor(route.anchor); }
      else void openArticle(route.article, { anchor: route.anchor, mode: 'none', view: route.view === 'history' ? 'history' : '' });
    } else {
      articleRequest.current++; setArticleLoading(false); setSelected(null); setHistoryView(false); setAuditView(route.view === 'audit'); setBooting(false);
      if (route.space && route.space !== spaceIdRef.current) { resetFilters(); setSpaceId(route.space); }
      else if (!spaceIdRef.current && session.mode !== 'corporate') setSpaceId('general');
    }
  };
  useEffect(() => {
    const onPopState = () => popRef.current();
    window.addEventListener('popstate', onPopState);
    return () => window.removeEventListener('popstate', onPopState);
  }, []);

  useEffect(() => { if (!selected || !pendingAnchor) return; const anchor = pendingAnchor; const frame = requestAnimationFrame(() => { document.getElementById(anchor)?.scrollIntoView({ behavior: smoothBehavior(), block: 'start' }); setPendingAnchor(''); }); return () => cancelAnimationFrame(frame); }, [selected?.id, selected?.body, pendingAnchor]);

  // ---- Search (/api/search).
  const searchActive = Boolean(query.trim() || tagFilter.trim() || propertyFilter.trim() || kindFilter || pinnedOnly);
  const searchParams = (offset = 0) => {
    const params: Record<string, string> = { limit: '30' };
    if (!searchAll) params.space_id = spaceId;
    if (query.trim()) params.q = query.trim(); if (tagFilter.trim()) params.tag = tagFilter.trim(); if (propertyFilter.trim()) params.property = propertyFilter.trim();
    if (kindFilter) params.kind = kindFilter; if (pinnedOnly) params.pinned = 'true'; if (offset) params.offset = String(offset);
    return params;
  };
  const immediateSearch = useRef(false);
  useEffect(() => {
    if (!searchActive || (!spaceId && !searchAll)) { immediateSearch.current = false; searchRequest.current++; setSearchResponse(null); setSearchItems([]); setSearchLoading(false); setSearchMoreBusy(false); return; }
    searchRequest.current++; let live = true; setSearchLoading(true); setSearchMoreBusy(false);
    const delay = immediateSearch.current ? 0 : 250; immediateSearch.current = false;
    const timer = window.setTimeout(async () => {
      const request = searchRequest.current;
      try { const result = await client.search(searchParams()); if (live && request === searchRequest.current) { setSearchResponse(result); setSearchItems(result.results); setSearchLoading(false); setSearchMoreBusy(false); } }
      catch (e) { if (live && request === searchRequest.current) { setError((e as Error).message); setSearchLoading(false); setSearchMoreBusy(false); } }
    }, delay);
    return () => { live = false; clearTimeout(timer); };
  }, [searchAll ? '' : spaceId, query, tagFilter, propertyFilter, kindFilter, pinnedOnly, searchAll, searchNonce]);
  const runSearch = () => { immediateSearch.current = true; setSearchNonce(n => n + 1); };
  const loadMoreSearch = async () => {
    if (!searchResponse || searchResponse.next_offset === undefined || searchMoreBusy) return;
    const request = searchRequest.current; setSearchMoreBusy(true);
    try {
      const result = await client.search(searchParams(searchResponse.next_offset));
      if (request !== searchRequest.current) return;
      setSearchResponse(prev => ({ ...result, mode: prev?.mode ?? result.mode })); setSearchItems(items => [...items, ...result.results.filter(hit => !items.some(item => item.id === hit.id))]);
    } catch (e) { if (request === searchRequest.current) setError((e as Error).message); }
    finally { setSearchMoreBusy(false); }
  };
  const resetSearch = () => { immediateSearch.current = true; resetFilters(); };

  useEffect(() => { if (!selected) return; let live = true;
    Promise.all([client.attachments(selected.id), client.links(selected.id)]).then(([a, l]) => { if (live) { setAttachments(a); setLinks(l); } }).catch(e => { if (live) setError((e as Error).message); });
    return () => { live = false; };
  }, [selected?.id, selected?.revision]);
  useEffect(() => {
    if (!pendingDelete && !pendingDeleteArticle) return;
    previousFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const frame = requestAnimationFrame(() => deleteCancelRef.current?.focus());
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') { event.preventDefault(); if (!deletingArticle && !deletingAttachmentId) { setPendingDelete(null); setPendingDeleteArticle(null); } return; }
      if (event.key !== 'Tab' || !deleteDialogRef.current) return;
      const controls = Array.from(deleteDialogRef.current.querySelectorAll<HTMLElement>('button:not(:disabled)'));
      if (!controls.length) return;
      const first = controls[0], last = controls[controls.length - 1];
      if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
    };
    document.addEventListener('keydown', onKeyDown);
    return () => { cancelAnimationFrame(frame); document.removeEventListener('keydown', onKeyDown); if (previousFocusRef.current?.isConnected) previousFocusRef.current.focus(); };
  }, [pendingDelete, pendingDeleteArticle, deletingArticle, deletingAttachmentId]);
  useEffect(() => {
    if (!selected) { setHistoryArticleId(''); setRevisions([]); setHistoryLoading(false); return; }
    let live = true;
    setHistoryArticleId(selected.id); setRevisions([]); setHistoryError(''); setHistoryLoading(true);
    client.history(selected.id).then(items => {
      if (!live) return;
      setRevisions(items);
    }).catch(e => { if (live) setHistoryError((e as Error).message); }).finally(() => { if (live) setHistoryLoading(false); });
    return () => { live = false; };
  }, [selected?.id, selected?.revision, historyReload]);

  // ---- Tree data.
  const articleById = useMemo(() => new Map(articles.map(a => [a.id, a])), [articles]);
  const tree = useMemo(() => {
    const map = new Map<string, Article[]>();
    articles.forEach(a => { const parent = a.parent_id && a.parent_id !== a.id && articleById.has(a.parent_id) ? a.parent_id : ''; const list = map.get(parent); if (list) list.push(a); else map.set(parent, [a]); });
    map.forEach(v => v.sort((a, b) => Number(b.pinned) - Number(a.pinned) || a.title.localeCompare(b.title, 'ru'))); return map;
  }, [articles, articleById]);
  const childArticles = useMemo(() => selected ? articles.filter(a => a.parent_id === selected.id && a.space_id === selected.space_id).sort((a, b) => a.title.localeCompare(b.title, 'ru')) : [], [articles, selected?.id, selected?.space_id]);
  const ancestors = useMemo(() => {
    const chain: { id: string; title: string }[] = []; if (!selected) return chain;
    const seen = new Set<string>([selected.id]); let parent = selected.parent_id;
    while (parent && !seen.has(parent)) { seen.add(parent); const a = articleById.get(parent); if (!a || a.space_id !== selected.space_id) break; chain.unshift({ id: a.id, title: a.title }); parent = a.parent_id; }
    return chain;
  }, [selected?.id, selected?.parent_id, selected?.space_id, articleById]);

  // Open the ancestors of the selected article and keep its node in view.
  const scrolledFor = useRef('');
  useEffect(() => {
    if (!selected) return;
    const chain: string[] = []; const seen = new Set<string>(); let parent = selected.parent_id;
    while (parent && !seen.has(parent)) { seen.add(parent); chain.push(parent); parent = articleById.get(parent)?.parent_id || ''; }
    if (chain.some(id => openNodes[id] === false)) setOpenNodes(v => { const next = { ...v }; chain.forEach(id => { if (next[id] === false) next[id] = true; }); return next; });
  }, [selected?.id, articleById]);
  useEffect(() => { scrolledFor.current = ''; }, [selected?.id, mobileNav]);
  useEffect(() => {
    if (!selected || searchActive || scrolledFor.current === selected.id) return;
    const node = document.querySelector('.tree-nav .tree-item.selected');
    if (node) { scrolledFor.current = selected.id; node.scrollIntoView({ block: 'nearest' }); }
  });

  // document.title
  useEffect(() => {
    const spaceTitle = spaces.find(s => s.id === spaceId)?.name;
    document.title = adminOpen ? 'Команды и доступ — AI Wiki' : selected && !editor && !auditView ? `${selected.title || "Без названия"} — AI Wiki` : spaceTitle ? `${spaceTitle} — AI Wiki` : "AI Wiki";
  }, [selected?.id, selected?.title, editor, auditView, spaces, spaceId, adminOpen]);

  // Hotkeys: «/» and Ctrl/Cmd+K focus the search; Escape closes the slide-out panels.
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        if (modalOpenRef.current) return;
        if (overlayRef.current.nav || overlayRef.current.panel) { setMobileNav(false); setPanel(null); }
        return;
      }
      const slash = event.key === '/' && !event.ctrlKey && !event.metaKey && !event.altKey;
      const ctrlK = (event.ctrlKey || event.metaKey) && !event.altKey && event.key.toLowerCase() === 'k';
      if ((!slash && !ctrlK) || modalOpenRef.current || isTypingTarget(event.target)) return;
      event.preventDefault();
      const focusSearch = () => { searchInputRef.current?.focus(); searchInputRef.current?.select(); };
      if (isNarrow()) setMobileNav(true); else focusSearch();
      requestAnimationFrame(focusSearch);
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, []);

  // ---- Navigation actions (all of them ask before dropping an unsaved draft).
  const leaveEditing = () => { setEditor(false); setDraft(null); baselineRef.current = null; setConflict(null); setOffer(null); setParkedList([]); setPropsAttempt(false); };
  const flushDraft = () => { const latest = latestDraftRef.current; if (latest) saveDraft(latest.key, latest.fields, latest.base); latestDraftRef.current = null; };
  /** Starts an editing session: remembers the storage key and offers a stored draft that differs from the opened article. */
  const startDraftSession = (key: string, baseline: DraftSnapshot, base: number) => {
    flushDraft(); draftKeyRef.current = key; setDraftBase(base); setConflict(null); setPropsAttempt(false);
    const stored = loadDraft(key);
    if (stored && !sameFields(stored.fields, baseline)) setOffer(stored); else { if (stored) removeDraft(key); setOffer(null); }
    setParkedList(loadParkedList(key));
  };
  // Only what belongs to the current editing session is removed; an offered draft nobody has looked at yet stays in storage.
  const discardDraft = () => { latestDraftRef.current = null; if (!offerRef.current) removeDraft(draftKeyRef.current); };
  const clearSelection = (updateURL = true) => { articleRequest.current++; attachmentDeleteRequest.current++; setArticleLoading(false); setPendingDelete(null); setDeletingAttachmentId(''); setSelected(null); leaveEditing(); setPanel(null); setHistoryView(false); setAuditView(false); historyEntry.current = false; if (updateURL) urlReplace(spaceUrl(spaceIdRef.current)); };
  const selectArticle = useCallback((id: string, anchor = '') => {
    if (!confirmLeave()) return;
    attachmentDeleteRequest.current++; setPendingDelete(null); setDeletingAttachmentId(''); setHistoryView(false); setAuditView(false); historyEntry.current = false;
    leaveEditing(); setPanel(null); setMobileNav(false);
    void openArticle(id, { anchor, mode: 'push' }).then(() => { if (!anchor) window.scrollTo(0, 0); });
  }, [confirmLeave, openArticle]);
  /** Search hits remember the query terms; they only apply while that very article is shown. */
  const openFromSearch = useCallback((id: string) => {
    setSearchHighlight({ id, terms: query.trim().split(/\s+/).filter(Boolean) }); pendingScrollRef.current = id;
    selectArticle(id);
  }, [query, selectArticle]);
  /** Internal links in the text: same article → just scroll, otherwise open it without a page reload. */
  const openInternal = useCallback((id: string, anchor: string) => {
    if (id === selectedIdRef.current && !editorOpenRef.current) {
      if (anchor) { document.getElementById(anchor)?.scrollIntoView({ behavior: smoothBehavior(), block: 'start' }); urlReplace(articleUrl(id, { anchor })); }
      else window.scrollTo({ top: 0, behavior: smoothBehavior() });
      return;
    }
    selectArticle(id, anchor);
  }, [selectArticle, urlReplace]);
  const goSpaceHome = (id: string) => {
    if (!confirmLeave()) return;
    clearSelection(false); setMobileNav(false);
    if (id !== spaceIdRef.current) { if (!searchAll) resetFilters(); setSpaceId(id); }
    urlPush(spaceUrl(id));
  };
  const changeSpace = (id: string) => {
    if (!confirmLeave()) return;
    clearSelection(false); resetFilters(); setSpaceId(id); urlPush(spaceUrl(id));
  };
  const closeEditor = () => { const wasDirty = dirtyRef.current; if (!confirmLeave()) return; if (wasDirty) discardDraft(); leaveEditing(); };
  const loadParentOptions = async (space: string) => { if (!space) { setParentOptions([]); return; } const request = ++parentRequest.current; const result: Article[] = []; try { for (let offset = 0; ; offset += 200) { const page = await client.articles({ space_id: space, limit: '200', offset: String(offset) }); if (request !== parentRequest.current) return; result.push(...page); if (page.length < 200) break; } setParentOptions(result); } catch (e) { if (request === parentRequest.current) setError((e as Error).message); } };
  const beginCreate = (parent = '') => {
    if (!spaces.some(s => s.id === spaceId && !s.archived_at)) { setSpaceManager(true); return; }
    if (editor && !confirmLeave()) return;
    articleRequest.current++; setArticleLoading(false); setHistoryView(false); setAuditView(false); historyEntry.current = false; setParentOptions(articles); setMobileNav(false);
    const value = { id: '', space_id: spaceId, project_id: (parent ? articles.find(a=>a.id===parent)?.project_id : '') || (spaces.find(s=>s.id===spaceId)?.limited ? projects.find(p=>p.space_id===spaceId&&!p.archived_at)?.id : '') || '', parent_id: parent, title: '', summary: '', body: '', kind: 'article', status: 'draft', pinned: false, tags: [], properties: {}, revision: 0, created_at: '', updated_at: '', author } as Article;
    baselineRef.current = snapshotOf(value, '', ''); startDraftSession(draftKey('', spaceId), baselineRef.current, 0); setTagsText(''); setPropertiesText(''); setDraft(value); setSelected(null); setEditor(true); setPreview(false); setPanel(null); urlPush(spaceUrl(spaceId));
  };
  const beginEdit = () => {
    if (selected && !selected.deleted_at) {
      setParentOptions(articles.filter(a => a.space_id === selected.space_id)); void loadParentOptions(selected.space_id);
      const tags = selected.tags.join(', '); const properties = Object.entries(selected.properties || {}).map(([k, v]) => `${k}=${v}`).join('\n');
      const value = { ...selected, tags: [...selected.tags], properties: { ...selected.properties } };
      baselineRef.current = snapshotOf(value, tags, properties); startDraftSession(draftKey(selected.id, selected.space_id), baselineRef.current, selected.revision); setTagsText(tags); setPropertiesText(properties); setDraft(value); setEditor(true); setPreview(false); setPanel(null);
    }
  };
  const setField = <K extends keyof Article>(key: K, value: Article[K]) => setDraft(d => d ? ({ ...d, [key]: value }) : d);
  const FIELD_NAMES: Record<keyof DraftFields, string> = { title: 'заголовок', summary: 'описание', body: 'текст', kind: 'тип', status: 'статус', pinned: 'закрепление', space_id: 'пространство', project_id: 'проект', parent_id: 'родитель', tags: 'теги', properties: 'свойства' };
  const fieldKeys = Object.keys(FIELD_NAMES) as (keyof DraftFields)[];
  const saveArticle = async (opts: { base?: number; skipInvalid?: boolean; notice?: string } = {}) => { if (!draft || !draft.title.trim() || saving) return;
    const { properties, invalid } = parseProperties(propertiesText);
    if (invalid.length && !opts.skipInvalid) { setPropsAttempt(true); const area = document.getElementById('editor-properties'); area?.scrollIntoView({ block: 'center' }); area?.focus(); return; }
    lastSkipRef.current = Boolean(opts.skipInvalid);
    const tags = tagsText.split(',').map(t => t.trim()).filter(Boolean);
    const full: Record<string, unknown> = { space_id: draft.space_id, project_id: draft.project_id || '', parent_id: draft.parent_id, title: draft.title.trim(), summary: draft.summary, body: draft.body, kind: draft.kind, status: draft.status, pinned: draft.pinned, tags, properties };
    // An existing article gets only the fields the user changed since the editor was opened (the server merges by present fields),
    // so concurrent edits of other fields are not overwritten.
    const base = baselineRef.current; let fields = full;
    if (draft.id && base) {
      const now = snapshotOf(draft, tagsText, propertiesText); fields = {};
      fieldKeys.forEach(key => { const value = key === 'title' ? draft.title.trim() : now[key]; if (value !== base[key]) fields[key] = key === 'tags' ? tags : key === 'properties' ? properties : value; });
      if (!Object.keys(fields).length) { discardDraft(); leaveEditing(); setNotice('Изменений нет'); return; }
    }
    setSaving(true); setError('');
    try { if (session.mode !== 'corporate') storeAuthor(author); const data = { ...fields, author: session.mode === 'corporate' ? session.user.name : author.trim() || 'human', ...(draft.id ? { base_revision: opts.base ?? draftBase, message: 'Изменения из редактора' } : {}) };
      const saved = draft.id ? await client.updateArticle(draft.id, data) : await client.createArticle(data); latestDraftRef.current = null; removeDraft(draftKeyRef.current); articleRequest.current++; setHistoryView(false); historyEntry.current = false; baselineRef.current = null; setSpaceId(saved.space_id); setSelected(saved); setDraft(null); setEditor(false); setConflict(null); setOffer(null); setParkedList([]); setPropsAttempt(false); setNotice(opts.notice || 'Изменения сохранены'); urlReplace(articleUrl(saved.id)); await loadIndex(saved.space_id); await loadTags(saved.space_id); setSelected(saved); void loadFavorites();
    } catch (e) {
      const message = (e as Error).message;
      if (draft.id && /revision conflict/i.test(message)) {
        try { setConflict(await client.article(draft.id)); }
        catch (loadError) { setError(`${message}. Не удалось загрузить текущую версию: ${(loadError as Error).message}`); }
      } else setError(message);
    } finally { setSaving(false); }
  };
  const currentFields = (): DraftFields => snapshotOf(draft as Article, tagsText, propertiesText);
  const applyStored = (stored: StoredDraft) => {
    if (!draft) return; const f = stored.fields;
    setDraft({ ...draft, title: f.title, summary: f.summary, body: f.body, kind: f.kind, status: f.status, pinned: f.pinned, space_id: f.space_id, project_id: f.project_id || '', parent_id: f.parent_id });
    setTagsText(f.tags); setPropertiesText(f.properties); setDraftBase(stored.base_revision);
    if (f.space_id !== draft.space_id) void loadParentOptions(f.space_id);
  };
  const restoreOffer = () => { if (!offer) return; if (dirtyRef.current && !window.confirm('Заменить текущие правки сохранённым черновиком?')) return; applyStored(offer); setOffer(null); };
  const deleteOffer = () => { if (!offer || !window.confirm('Удалить сохранённый черновик? Это нельзя отменить.')) return; removeDraft(draftKeyRef.current); setOffer(null); };
  const restoreParked = () => { if (!parked) return; if (dirtyRef.current && !window.confirm('Заменить текущие правки отложенным черновиком?')) return; applyStored(parked); setParkedList(popParked(draftKeyRef.current)); };
  const deleteParked = () => { if (!parked || !window.confirm('Удалить отложенный черновик? Это нельзя отменить.')) return; setParkedList(popParked(draftKeyRef.current)); };
  /** «Take their version»: the user's text is parked (kept, restorable) and the editor is rebased onto the server version. */
  const takeTheirs = () => {
    if (!conflict || !draft) return; const key = draftKeyRef.current; const fields = currentFields();
    const stored: StoredDraft = { fields, base_revision: draftBase, savedAt: Date.now() };
    if (!pushParked(key, stored)) setNotice('Браузер не дал сохранить отложенный черновик: он доступен только пока открыта эта страница');
    setParkedList(list => list.length && sameFields(list[0].fields, fields) ? list : [stored, ...list].slice(0, 5));
    const tags = conflict.tags.join(', '); const props = Object.entries(conflict.properties || {}).map(([k, v]) => `${k}=${v}`).join('\n');
    const value = { ...conflict, tags: [...conflict.tags], properties: { ...conflict.properties } };
    baselineRef.current = snapshotOf(value, tags, props); latestDraftRef.current = null; removeDraft(key); setOffer(null);
    setTagsText(tags); setPropertiesText(props); setDraft(value); setDraftBase(conflict.revision); setSelected(s => s && s.id === conflict.id ? conflict : s);
    setParentOptions(articles.filter(a => a.space_id === conflict.space_id)); void loadParentOptions(conflict.space_id); setConflict(null);
  };
  /** «Save over»: sends only my changed fields on top of the current version; asks only if the same fields were changed on the server too. */
  const overwriteConflict = () => {
    if (!conflict || !draft) return; const base = baselineRef.current; const n = conflict.revision;
    const theirs = snapshotOf(conflict, conflict.tags.join(', '), Object.entries(conflict.properties || {}).map(([k, v]) => `${k}=${v}`).join('\n')); const mine = snapshotOf(draft, tagsText, propertiesText);
    const overlap = base ? fieldKeys.filter(k => mine[k] !== base[k] && theirs[k] !== base[k] && mine[k] !== theirs[k]) : fieldKeys.filter(k => mine[k] !== theirs[k]);
    if (overlap.includes('body')) { if (!window.confirm(`Ваш текст заменит изменения версии ${n} в тексте статьи. Продолжить?`)) return; }
    else if (overlap.length && !window.confirm(`Ваши значения (${overlap.map(k => FIELD_NAMES[k]).join(', ')}) заменят изменения версии ${n}. Продолжить?`)) return;
    setDraftBase(n);
    void saveArticle({ base: n, skipInvalid: lastSkipRef.current, notice: overlap.length ? undefined : `Ваши изменения применены поверх версии ${n}` });
  };
  const saveRef = useRef(saveArticle); saveRef.current = saveArticle; const closeRef = useRef(closeEditor); closeRef.current = closeEditor; const savingRef = useRef(saving); savingRef.current = saving;
  // Ctrl/Cmd+S saves (capture phase, so it also works inside CodeMirror); Esc closes the editor through the unsaved-changes guard.
  useEffect(() => {
    if (!editor) return;
    const onSave = (event: KeyboardEvent) => {
      if (!(event.ctrlKey || event.metaKey) || event.altKey || event.shiftKey || !(event.code === 'KeyS' || event.key.toLowerCase() === 's')) return;
      event.preventDefault(); if (modalOpenRef.current || savingRef.current) return; void saveRef.current();
    };
    const onEscape = (event: KeyboardEvent) => {
      if (event.key !== 'Escape' || event.defaultPrevented || event.isComposing || modalOpenRef.current || savingRef.current || overlayRef.current.nav || overlayRef.current.panel) return;
      const target = event.target;
      if (!(target instanceof Element) || !target.closest('.editor-page') || target.matches('input[list]')) return;
      if (document.querySelector('[role="dialog"], .cm-tooltip-autocomplete, .parent-picker-list')) return;
      closeRef.current();
    };
    document.addEventListener('keydown', onSave, true); document.addEventListener('keydown', onEscape);
    return () => { document.removeEventListener('keydown', onSave, true); document.removeEventListener('keydown', onEscape); };
  }, [editor]);
  const addSpace = async (e: React.FormEvent) => { e.preventDefault(); if (!spaceName.trim()) return; if (!confirmLeave()) return; try { const s = await client.createSpace(spaceName.trim(), spaceDescription.trim()); setSpaces(v => [...v, s]); clearSelection(false); resetFilters(); setSpaceId(s.id); urlPush(spaceUrl(s.id)); setSpaceForm(false); setSpaceName(''); setSpaceDescription(''); } catch (e) { setError((e as Error).message); } };
  const uploadFile = async (files: FileList | null) => { if (!files?.length || !selected || selected.deleted_at) return; try { const added = await client.upload(selected.id, files[0]); setAttachments(v => [added, ...v]); if (draft) { if (added.mime.startsWith('image/')) insertText(`![${added.name}](${added.url})`); else insertText(`[${added.name}](${added.url})`); setNotice('Файл загружен и ссылка вставлена в текст'); } else setNotice('Файл добавлен к статье'); } catch (e) { setError((e as Error).message); } };
  const insertText = (value: string) => { if (!draft) return; if (bodyRef.current) bodyRef.current.insertText(value); else setField('body', draft.body + value); };
  const deleteAttachment = async () => { if (!selected || selected.deleted_at || !pendingDelete || deletingAttachmentId) return; const articleId = selected.id; const attachment = pendingDelete; const request = ++attachmentDeleteRequest.current; setDeletingAttachmentId(attachment.id); setError('');
    try { await client.deleteAttachment(articleId, attachment.id); if (request !== attachmentDeleteRequest.current || selectedIdRef.current !== articleId) return; setAttachments(items => items.filter(item => item.id !== attachment.id)); setPendingDelete(null); setNotice(`Файл «${attachment.name}» удалён из статьи`); }
    catch (e) { if (request === attachmentDeleteRequest.current && selectedIdRef.current === articleId) setError((e as Error).message); }
    finally { if (request === attachmentDeleteRequest.current) setDeletingAttachmentId(''); }
  };
  const deleteArticle = async () => { if (!selected || selected.deleted_at || deletingArticle) return; const articleId = selected.id; setDeletingArticle(true); setError('');
    try { if (session.mode !== 'corporate') storeAuthor(author); const deleted = await client.deleteArticle(articleId, { base_revision: selected.revision, author: session.mode === 'corporate' ? session.user.name : author.trim() || 'human' }); if (selectedIdRef.current !== articleId) return; setSelected(deleted); setPendingDeleteArticle(null); await loadIndex(deleted.space_id); void loadFavorites(); }
    catch (e) { if (selectedIdRef.current === articleId) setError((e as Error).message); }
    finally { setDeletingArticle(false); }
  };
  const undeleteArticle = async () => { if (!selected?.deleted_at || undeletingArticle) return; const articleId = selected.id; setUndeletingArticle(true); setError('');
    try { if (session.mode !== 'corporate') storeAuthor(author); const restored = await client.undeleteArticle(articleId, { base_revision: selected.revision, author: session.mode === 'corporate' ? session.user.name : author.trim() || 'human' }); if (selectedIdRef.current !== articleId) return; setSelected(restored); await loadIndex(restored.space_id); void loadFavorites(); }
    catch (e) { if (selectedIdRef.current === articleId) setError((e as Error).message); }
    finally { setUndeletingArticle(false); }
  };
  const openHistory = () => { if (!selected || editorOpenRef.current) return; setAuditView(false); historyEntry.current = true; urlPush(articleUrl(selected.id, { view: 'history' })); setHistoryView(true); setPanel(null); };
  const openAudit = () => { if (!spaceId) return; if (auditView) { setMobileNav(false); return; } if (!confirmLeave()) return; clearSelection(false); setAuditView(true); setMobileNav(false); urlPush(spaceUrl(spaceId, 'audit')); };
  const openAuditHistory = (id: string) => { if (!confirmLeave()) return; setPanel(null); void openArticle(id, { mode: 'push', view: 'history', entry: true }); };
  const closeHistory = () => { if (historyEntry.current) { history.back(); return; } if (!selected) return; urlReplace(articleUrl(selected.id)); setHistoryView(false); };
  const restore = async (revision: Revision) => { if (!selected || selected.deleted_at || restoring) return; setRestoring(true); setError(''); try { const a = await client.restore(selected.id, { revision: revision.revision, base_revision: selected.revision, author: author.trim() || 'human', message: `Восстановление версии ${revision.revision}` }); setSpaceId(a.space_id); setSelected(a); await loadIndex(a.space_id); setSelected(a); void loadFavorites(); setNotice(`Создана новая версия из ревизии ${revision.revision}`); } catch (e) { setError((e as Error).message); } finally { setRestoring(false); } };
  const exportWiki = () => { window.location.href = '/api/export'; };
  const toggleTheme = () => {
    const next = theme === 'dark' ? 'light' : 'dark';
    setTheme(next);
    document.documentElement.dataset.theme = next;
    document.documentElement.style.colorScheme = next;
    try { localStorage.setItem('aiwiki.theme', next); } catch { /* Theme remains active for this session. */ }
  };

  // ---- Favourites (optimistic, rolled back on error).
  const isFavorite = Boolean(selected && favorites.some(f => f.id === selected.id));
  const toggleFavorite = async () => {
    if (!selected || favPending.includes(selected.id)) return;
    const article = selected; const existing = favorites.find(f => f.id === article.id);
    setFavPending(v => [...v, article.id]);
    if (existing) setFavorites(v => v.filter(f => f.id !== article.id));
    else setFavorites(v => [{ id: article.id, title: article.title, space_id: article.space_id, parent_id: article.parent_id, kind: article.kind, status: article.status, summary: article.summary, updated_at: article.updated_at, favorited_at: new Date().toISOString() }, ...v]);
    try { if (existing) await client.removeFavorite(article.id); else await client.addFavorite(article.id); }
    catch (e) {
      if (existing) setFavorites(v => v.some(f => f.id === existing.id) ? v : [...v, existing].sort((a, b) => b.favorited_at.localeCompare(a.favorited_at)));
      else setFavorites(v => v.filter(f => f.id !== article.id));
      setError(`Не удалось ${existing ? 'убрать из избранного' : 'добавить в избранное'}: ${(e as Error).message}`);
    } finally { setFavPending(v => v.filter(id => id !== article.id)); }
  };

  const activeSpaces = spaces.filter(s => !s.archived_at);
  const updateSpace = (updated: Space) => {
    void loadFavorites();
    const nextSpaces = spaces.some(s => s.id === updated.id) ? spaces.map(s => s.id === updated.id ? updated : s) : [...spaces, updated];
    setSpaces(nextSpaces);
    if ((updated.id === spaceId && updated.archived_at && !spaces.find(s => s.id === updated.id)?.archived_at) || !spaceId) {
      indexRequest.current++; searchRequest.current++; parentRequest.current++; tagRequest.current++;
      const next = nextSpaces.find(s => !s.archived_at)?.id || '';
      if (!editor) clearSelection();
      setArticles([]); setParentOptions([]); setTagOptions([]); setBootstrap(null); setBusy(false); setSpaceId(next);
    }
  };
  const activeSpace = spaces.find(s => s.id === spaceId);
  const propsInvalid = editor ? parseProperties(propertiesText).invalid : [];
  const editorTagComma = tagsText.lastIndexOf(','); const editorTagPrefix = editorTagComma < 0 ? '' : tagsText.slice(0, editorTagComma + 1) + ' '; const editorTagToken = (editorTagComma < 0 ? tagsText : tagsText.slice(editorTagComma + 1)).trim().toLocaleLowerCase('ru');
  const editorTagSuggestions = tagOptions.filter(tag => tag.toLocaleLowerCase('ru').startsWith(editorTagToken)).map(tag => `${editorTagPrefix}${tag}`);
  const onToggleNode = useCallback((id: string, expanded: boolean) => setOpenNodes(v => ({ ...v, [id]: !expanded })), []);
  const onTreeSelect = useCallback((id: string) => selectArticle(id), [selectArticle]);
  const bodyForRead = selected?.body || '';
  const pageTitle = selected?.title || 'Ваша база знаний';
  const toc = useMemo(() => headings(bodyForRead), [bodyForRead]);
  const articleHighlight = selected && searchHighlight.id === selected.id && searchHighlight.terms.length ? searchHighlight.terms : undefined;
  // After opening an article from search, bring the first highlighted match into view (selectArticle scrolls to top first).
  useEffect(() => {
    if (!articleHighlight || location.hash || pendingScrollRef.current !== selected?.id) return;
    const timer = window.setTimeout(() => { pendingScrollRef.current = ''; scrollToFirstHighlight(document.querySelector('.article-content') || document, smoothBehavior()); }, 150);
    return () => window.clearTimeout(timer);
  }, [articleHighlight, selected?.id]);
  const openComment = useCallback((anchor = '') => { if (selectedRef.current?.deleted_at || editorOpenRef.current) return; setPanel(null); requestAnimationFrame(() => { document.getElementById('article-comments')?.scrollIntoView({ behavior: smoothBehavior(), block: 'start' }); commentsRef.current?.openComment(anchor); }); }, []);
  const onHeading = useCallback((id: string) => openComment(id), [openComment]);
  const onCommentCount = useCallback((open: number, total: number) => setCommentStats(prev => prev.open === open && prev.total === total ? prev : { open, total }), []);
  const commentsText = commentsLabel(commentStats.open, commentStats.total);
  const showArticleLoading = articleLoading && selected?.id !== loadingId;
  const tocPanelOpen = panel === 'toc';

  const changeAdminTab = (tab: Parameters<typeof adminUrl>[0] = 'catalog') => {
    urlPush(adminUrl(tab));
    history.replaceState({ aiwikiAdminReturn: adminReturnUrl.current }, '', currentUrl());
    setAdminTab(tab);
  };
  const openAdmin = () => {
    if (!confirmLeave()) return;
    adminReturnUrl.current = currentUrl();
    articleRequest.current++; setArticleLoading(false);
    leaveEditing(); setPanel(null); setMobileNav(false);
    changeAdminTab(); setAdminOpen(true);
  };
  const closeAdmin = () => {
    urlPush(adminReturnUrl.current);
    popRef.current();
    void loadIndex(spaceId);
    void client.projects().then(setProjects).catch(() => setProjects([]));
  };
  if (adminOpen && session.mode === 'corporate') return <AdminPanel
    isAdmin={session.user.admin} userId={session.user.id} activeTab={adminTab}
    onTabChange={changeAdminTab} onClose={closeAdmin}/>;

  return <div className="app-shell">
    <header className="topbar"><button className="mobile-menu icon-button" onClick={() => setMobileNav(!mobileNav)} aria-label="Меню" aria-expanded={mobileNav}><ListFilter size={19}/></button><a className="brand" href="/" onClick={e => { e.preventDefault(); goSpaceHome(spaceId); }}><span className="brand-mark"><BookOpen size={19}/></span><span>ai<span className="brand-accent">wiki</span></span></a><div className="topbar-divider"/><div className="crumbs"><span>{activeSpace?.name || 'Нет активного пространства'}</span></div><div className="top-actions"><label className="author-chip"><span>Автор</span><input value={session.mode === 'corporate' ? session.user.name : author} readOnly={session.mode === 'corporate'} onChange={e => { setAuthor(e.target.value); storeAuthor(e.target.value); }} placeholder="Ваше имя" aria-label="Имя автора"/></label>{session.mode === 'corporate' && <><button className="icon-button" onClick={openAdmin} title="Команды и доступ" aria-label="Команды и доступ"><ShieldCheck size={18}/></button><button className="button ghost small" onClick={onLogout}>Выйти</button></>}<button className="button ghost small" onClick={exportWiki} title="Экспорт JSON"><ArrowDownToLine size={16}/>Экспорт</button><button className="icon-button theme-toggle" onClick={toggleTheme} title={theme === 'dark' ? 'Включить светлую тему' : 'Включить тёмную тему'} aria-label={theme === 'dark' ? 'Включить светлую тему' : 'Включить тёмную тему'}>{theme === 'dark' ? <Sun size={18}/> : <Moon size={18}/>}</button><button className="icon-button" onClick={() => setPanel(panel === 'ai' ? null : 'ai')} title="Подключить ИИ" aria-label="Подключить ИИ"><Bot size={19}/></button></div></header>
    {logoutError && <div className="error-alert logout-error" role="alert">Не удалось завершить сеанс: {logoutError}</div>}
    <div ref={sidebar.workspaceRef} className={`workspace ${historyView ? 'history-workspace' : ''} ${auditView ? 'audit-workspace' : ''} ${sidebar.width !== null ? 'left-custom' : ''}`} style={sidebar.width !== null ? { '--left-w': `${sidebar.width}px` } as React.CSSProperties : undefined}>
      {mobileNav && <div className="drawer-backdrop for-nav" onClick={() => setMobileNav(false)} aria-hidden="true"/>}
      {panel && <div className="drawer-backdrop for-panel" onClick={() => setPanel(null)} aria-hidden="true"/>}
      {sidebar.handleLeft !== null && <div className={`sidebar-resizer ${sidebar.dragging ? 'dragging' : ''}`} role="separator" aria-orientation="vertical" aria-label="Ширина левой панели" aria-valuemin={SIDEBAR_MIN} aria-valuemax={SIDEBAR_MAX} aria-valuenow={sidebar.shownWidth} tabIndex={0} title="Потяните, чтобы изменить ширину. Двойной щелчок — сбросить" style={{ left: sidebar.handleLeft - 3 }} onPointerDown={sidebar.onPointerDown} onKeyDown={sidebar.onKeyDown} onDoubleClick={sidebar.reset}/>}
      <aside ref={sidebar.sidebarRef} className={`sidebar left-sidebar ${mobileNav ? 'mobile-open' : ''}`}>
        <div className="side-heading"><span>ПРОСТРАНСТВО</span><button className="icon-button tiny" onClick={() => setSpaceForm(!spaceForm)} title="Создать пространство"><FolderPlus size={16}/></button></div>
        <div className="space-picker"><select value={spaceId} onChange={e => changeSpace(e.target.value)} aria-label="Пространство">{!spaceId && <option value="">Нет активных пространств</option>}{activeSpace?.archived_at && <option value={activeSpace.id}>{activeSpace.name} · В архиве</option>}{activeSpaces.map(s => <option key={s.id} value={s.id}>{s.name}</option>)}</select><ChevronDown size={15}/></div>
        <button className="spaces-manage-button" onClick={() => { setSpaceManager(true); setMobileNav(false); }}><Settings2 size={15}/>Управление пространствами</button>
        {spaceId && <button className={`spaces-manage-button audit-nav-button ${auditView ? 'active' : ''}`} onClick={openAudit}><History size={15}/>Журнал изменений</button>}
        {spaceId && <button className="spaces-manage-button" onClick={() => { setImportOpen(true); setMobileNav(false); }}><Upload size={15}/>Импорт Markdown</button>}
        {spaceForm && <form className="space-form" onSubmit={e => void addSpace(e)}><input autoFocus value={spaceName} onChange={e => setSpaceName(e.target.value)} placeholder="Название"/><input value={spaceDescription} onChange={e => setSpaceDescription(e.target.value)} placeholder="Описание (необязательно)"/><div><button className="button primary small">Создать</button><button type="button" className="button ghost small" onClick={() => setSpaceForm(false)}>Отмена</button></div></form>}
        <button className="create-button" onClick={() => beginCreate()}><Plus size={16}/>Новая статья</button>
        <div className="search-box"><Search size={16}/><input ref={searchInputRef} value={query} onChange={e => setQuery(e.target.value)} onKeyDown={e => e.key === 'Enter' && runSearch()} placeholder="Поиск (нажмите /)" aria-label="Поиск"/><kbd>↵</kbd></div>
        <label className="search-scope"><input type="checkbox" checked={searchAll} onChange={e => setSearchAll(e.target.checked)}/>Во всех пространствах</label>
        <div className="filter-row"><input list="space-tag-options" value={tagFilter} onChange={e => setTagFilter(e.target.value)} onKeyDown={e => e.key === 'Enter' && runSearch()} placeholder="Тег" title="Фильтр по тегу"/><datalist id="space-tag-options">{tagOptions.map(tag => <option key={tag} value={tag}/>)}</datalist><input value={propertyFilter} onChange={e => setPropertyFilter(e.target.value)} onKeyDown={e => e.key === 'Enter' && runSearch()} placeholder="ключ:значение" title="Фильтр по свойству"/></div>
        <div className="filter-row"><select value={kindFilter} onChange={e => setKindFilter(e.target.value)} aria-label="Тип материала"><option value="">Все типы</option>{kinds.map(k => <option key={k} value={k}>{kindNames[k]}</option>)}</select><button className={`filter-toggle ${pinnedOnly ? 'active' : ''}`} onClick={() => setPinnedOnly(!pinnedOnly)} title="Только закреплённые" aria-pressed={pinnedOnly}><Pin size={14}/></button><button className="button ghost tiny" onClick={runSearch}>Найти</button><button className="button ghost tiny" onClick={resetSearch} title="Сбросить фильтры">Сброс</button></div>
        <FavoritesBlock favorites={favorites} spaces={spaces} currentSpace={spaceId} selectedId={selected?.id || ''} onOpen={selectArticle}/>
        <div className="tree-caption"><span>{searchActive ? 'РЕЗУЛЬТАТЫ' : 'МАТЕРИАЛЫ'}</span><span>{searchActive ? (searchResponse ? searchResponse.total : '') : articles.length}</span></div>
        <nav className="tree-nav">{searchActive ? <SearchResultsList response={searchResponse} results={searchItems} loading={searchLoading} loadingMore={searchMoreBusy} global={searchAll} spaces={spaces} currentSpace={spaceId} articleById={articleById} selectedId={selected?.id || ''} onOpen={openFromSearch} onMore={() => void loadMoreSearch()} onReset={resetSearch}/> : busy ? <div className="loading-line">Загружаем материалы…</div> : articles.length ? <ArticleTree tree={tree} openNodes={openNodes} selectedId={selected?.id || ''} onToggle={onToggleNode} onSelect={onTreeSelect}/> : <div className="empty-tree"><BookOpen size={18}/><span>Здесь пока пусто</span><button onClick={() => beginCreate()}>Создать первую статью</button></div>}</nav>
        <div className="sidebar-bottom"><button onClick={() => setPanel(panel === 'ai' ? null : 'ai')}><Sparkles size={15}/>Подключить ИИ</button><span className="connection-dot"/>База знаний</div>
      </aside>
      <main className="reading-area">
        {error && <div className="alert error-alert"><CircleHelp size={17}/><span>{error}</span><button onClick={() => setError('')} aria-label="Закрыть"><X size={16}/></button></div>}{notice && <div className="alert success-alert"><Check size={16}/>{notice}<button onClick={() => setNotice('')} aria-label="Закрыть"><X size={15}/></button></div>}
        {auditView && spaceId ? <AuditPage key={spaceId} spaceId={spaceId} spaceName={activeSpace?.name || spaceId} onOpenArticle={id => selectArticle(id)} onOpenHistory={id => openAuditHistory(id)}/> : historyView && selected ? <HistoryPage article={selected} revisions={historyArticleId === selected.id ? revisions : []} loading={historyArticleId !== selected.id || historyLoading} error={historyArticleId === selected.id ? historyError : ''} restoring={restoring} deleted={Boolean(selected.deleted_at)} onRetry={() => setHistoryReload(value => value + 1)} onBack={closeHistory} onRestore={revision => void restore(revision)}/> : editor && draft ? <section className={`editor-page${splitActive ? ' is-split' : ''}`}><div className="editor-top"><div><div className="eyebrow">{draft.id ? 'РЕДАКТИРОВАНИЕ' : 'НОВЫЙ МАТЕРИАЛ'}{isDirty && <span className="dirty-mark"> · есть несохранённые изменения</span>}</div><h1>{draft.id ? 'Изменить статью' : 'Создать статью'}</h1></div><button className="icon-button" onClick={closeEditor} title="Закрыть (Esc)"><X size={19}/></button></div>
          <div ref={conflictRef}>{conflict && mineFields && <EditorConflict current={conflict} mine={mineFields} busy={saving} onTakeTheirs={takeTheirs} onOverwrite={overwriteConflict} onDismiss={() => setConflict(null)}/>}</div>
          {offer && <div className="draft-banner info" role="status"><div>Найден несохранённый черновик от {formatDraftTime(offer.savedAt)}.{draft.id && offer.base_revision < draft.revision && <> Статья с тех пор менялась (черновик начат с версии {offer.base_revision}, сейчас {draft.revision}): при сохранении появится окно сравнения.</>}</div><div className="draft-banner-actions"><button type="button" className="button primary small" onClick={restoreOffer}>Восстановить</button><button type="button" className="button ghost small" onClick={deleteOffer}>Удалить</button></div></div>}
          {parked && <div className="draft-banner" role="status"><div>Ваш черновик от {formatDraftTime(parked.savedAt)} отложен (основан на версии {parked.base_revision}).{parkedList.length > 1 && <> Ещё отложенных: {parkedList.length - 1}.</>}</div><div className="draft-banner-actions"><button type="button" className="button small" onClick={restoreParked}>Вернуть отложенный</button><button type="button" className="button ghost small" onClick={deleteParked}>Удалить</button></div></div>}
          <div className="form-grid"><label className="field wide"><span>Заголовок <b>*</b></span><input maxLength={300} autoFocus value={draft.title} onChange={e => setField('title', e.target.value)} placeholder="Например, как устроен проект"/></label><label className="field wide"><span>Краткое описание</span><input value={draft.summary} onChange={e => setField('summary', e.target.value)} placeholder="Одно предложение для поиска и навигации"/></label>
            <label className="field"><span>Пространство</span><select value={draft.space_id} onChange={e => { setField('space_id', e.target.value); setField('project_id', ''); setField('parent_id', ''); void loadParentOptions(e.target.value); }}>{!activeSpaces.some(s => s.id === draft.space_id) && <option value={draft.space_id} disabled>{spaces.find(s => s.id === draft.space_id)?.name || 'Пространство'} · В архиве</option>}{activeSpaces.map(s => <option key={s.id} value={s.id}>{s.name}</option>)}</select>{draft.id && draft.space_id !== selected?.space_id && <small className="move-space-hint">Вместе со статьёй перенесутся все дочерние материалы.</small>}</label>{projects.length > 0 && <label className="field"><span>Проект</span><select value={draft.project_id || ''} onChange={e => { setField('project_id', e.target.value); setField('parent_id', ''); }}><option value="">Без проекта</option>{projects.filter(p => !p.archived_at && p.space_id === draft.space_id).map(p => <option key={p.id} value={p.id}>{p.name}</option>)}</select></label>}<div className="field"><span>Родительская статья</span><ParentPicker options={parentOptions.filter(a => a.space_id === draft.space_id && (a.project_id || "") === (draft.project_id || ""))} value={draft.parent_id} excludeId={draft.id} onChange={id => setField('parent_id', id)}/></div>
            <label className="field"><span>Тип</span><select value={draft.kind} onChange={e => setField('kind', e.target.value)}>{kinds.map(k => <option key={k} value={k}>{kindNames[k]}</option>)}</select></label><label className="field"><span>Статус</span><select value={draft.status} onChange={e => setField('status', e.target.value)}>{statuses.map(s => <option key={s} value={s}>{statusNames[s]}</option>)}</select></label>
            <label className="field wide"><span>Теги <small>через запятую</small></span><input list="editor-tag-options" value={tagsText} onChange={e => setTagsText(e.target.value)} placeholder="исследование, архитектура"/><datalist id="editor-tag-options">{editorTagSuggestions.map(tag => <option key={tag} value={tag}/>)}</datalist></label><label className="field wide"><span>Свойства <small>ключ=значение, по одному на строку</small></span><textarea id="editor-properties" className={`properties-area${propsAttempt && propsInvalid.length ? ' invalid' : ''}`} value={propertiesText} onChange={e => setPropertiesText(e.target.value)} placeholder="owner=команда\nsource=https://example.org" rows={2}/>{propsInvalid.length > 0 && <div className="props-warning" role="alert"><b>Эти строки не будут сохранены: в них нет «ключ=значение».</b><ul>{propsInvalid.map((line, i) => <li key={i}>{line}</li>)}</ul><div className="props-warning-actions"><button type="button" className="button small" onClick={() => document.getElementById('editor-properties')?.focus()}>Исправить</button><button type="button" className="button ghost small" disabled={saving || !draft.title.trim()} onClick={() => void saveArticle({ skipInvalid: true })}>Сохранить без них</button></div></div>}</label>
            <label className="pin-check"><input type="checkbox" checked={draft.pinned} onChange={e => setField('pinned', e.target.checked)}/><Pin size={15}/>Закрепить материал в инструкциях для ИИ</label>
          </div>
          <div className="editor-body-head"><div><strong>Содержимое</strong><small>Markdown · поддерживаются таблицы, задачи и ссылки</small></div><div className="editor-controls"><button className={`button ${!preview && !splitActive ? 'selected-tab' : ''}`} onClick={() => { setPreview(false); setSplit(false); }}><Pencil size={14}/>Редактор</button><button className={`button ${preview && !splitActive ? 'selected-tab' : ''}`} onClick={() => { setPreview(true); setSplit(false); }}><BookOpen size={14}/>Просмотр</button><button className={`button split-toggle ${splitActive ? 'selected-tab' : ''}`} aria-pressed={splitActive} onClick={() => { setSplit(true); setPreview(false); }} title="Редактор и предпросмотр в две колонки"><Columns2 size={14}/>Рядом</button><button className="button ghost" onClick={() => uploadRef.current?.click()} disabled={!draft.id} title={draft.id ? 'Загрузить файл' : 'Сначала сохраните статью'}><ImagePlus size={15}/>Файл</button><input ref={uploadRef} type="file" hidden onChange={e => void uploadFile(e.target.files)}/></div></div>
          <div className={`editor-split${splitActive ? ' is-split' : ''}`}><div className="editor-split-pane" hidden={preview && !splitActive}><Suspense fallback={<div className="body-editor" aria-busy="true"/>}><MarkdownEditor key={draft.id || 'new'} ref={bodyRef} value={draft.body} onChange={value => setField('body', value)} theme={theme} /></Suspense></div>
          {splitActive && <div className="editor-split-preview"><div className="preview-box"><Markdown body={previewBody} onNavigate={openInternal}/>{!draft.body && <span className="muted">Предпросмотр появится здесь.</span>}</div></div>}</div>
          {preview && !splitActive && <div className="preview-box"><Markdown body={draft.body} onNavigate={openInternal}/>{!draft.body && <span className="muted">Предпросмотр появится здесь.</span>}</div>}
          <div className="editor-footer"><span>{draft.body.length.toLocaleString('ru-RU')} символов{draft.id ? ` · версия ${draft.revision}` : ''}</span><div><button className="button ghost" onClick={closeEditor}>Отмена</button><button className="button primary" onClick={() => void saveArticle()} disabled={saving || !draft.title.trim()} title="Ctrl+S">{saving ? 'Сохраняем…' : <><Check size={16}/>Сохранить</>}</button></div></div>
        </section> : showArticleLoading ? <section className="article-loading" role="status" aria-live="polite"><div className="loading-bar"/><p>Загружаем статью…</p></section> : selected ? <article className="article-page"><div className="article-actions"><Breadcrumbs spaceName={activeSpace?.name || 'Пространство'} ancestors={ancestors} current={selected.title} onSpace={() => goSpaceHome(selected.space_id)} onOpen={id => selectArticle(id)}/><div className="action-buttons"><button type="button" className={`icon-button favorite-button ${isFavorite ? 'active' : ''}`} aria-pressed={isFavorite} aria-label={isFavorite ? 'Убрать из избранного' : 'В избранное'} title={isFavorite ? 'Убрать из избранного' : 'В избранное'} disabled={favPending.includes(selected.id)} onClick={() => void toggleFavorite()}><Star size={17} fill={isFavorite ? 'currentColor' : 'none'}/></button><button type="button" className="button ghost mobile-toc-button" onClick={() => setPanel(tocPanelOpen ? null : 'toc')} aria-expanded={tocPanelOpen}><List size={15}/>Оглавление</button><button className="button ghost" onClick={openHistory}><History size={15}/>История</button><ArticleExportMenu article={selected} spaceName={activeSpace?.name}/><button className="button ghost comments-button" onClick={() => document.getElementById('article-comments')?.scrollIntoView({ behavior: smoothBehavior(), block: 'start' })} title={commentsText}><MessageCircle size={15}/>Комментарии <span className="count">{commentsText}</span></button>{!selected.deleted_at && <><button className="button primary edit-button" onClick={beginEdit}><Pencil size={15}/>Редактировать</button><button type="button" className="icon-button article-delete-button" aria-label="Удалить статью" title="Удалить статью" onClick={() => setPendingDeleteArticle(selected)}><Trash2 size={17}/></button></>}</div></div>
          {activeSpace?.archived_at && <section className="space-archive-banner"><Archive size={17}/><span>Пространство «{activeSpace.name}» в архиве. Материалы сохранены.</span><button className="button ghost small" onClick={() => setSpaceManager(true)}>Управление</button></section>}
          {selected.deleted_at && <section className="deleted-banner" role="status"><div><strong>Статья удалена</strong><p>Она скрыта из дерева и поиска. История, файлы и комментарии сохранены.</p></div><button className="button primary" onClick={() => void undeleteArticle()} disabled={undeletingArticle}>{undeletingArticle ? 'Восстанавливаем…' : 'Восстановить статью'}</button></section>}
          <div className="article-meta"><span className={`status-pill status-${selected.status}`}>{selected.status === 'verified' && <ShieldCheck size={13}/>}<i/>{statusNames[selected.status] || selected.status}</span>{selected.pinned && <span className="pinned-pill"><Pin size={12}/>Закреплено</span>}<span className="meta-dot">·</span><span>Версия {selected.revision}</span><span className="meta-dot">·</span><span>Обновлено {prettyDate(selected.updated_at)}</span>{selected.author && <><span className="meta-dot">·</span><span>{selected.author}</span></>}</div>
          <h1 className="article-title">{pageTitle}</h1>{selected.summary && <p className="article-summary">{selected.summary}</p>}
          <div className="tag-list">{selected.tags.map(t => <button key={t} className="tag-chip" onClick={() => { setTagFilter(t); if (isNarrow()) setMobileNav(true); }}><Tag size={12}/>{t}</button>)}{Object.entries(selected.properties || {}).map(([k, v]) => <span className="property-chip" key={k}>{k}: <b>{v}</b></span>)}</div>
          {!selected.deleted_at && <div className="article-content"><Markdown body={bodyForRead} onHeading={onHeading} onNavigate={openInternal} highlight={articleHighlight}/>{!bodyForRead && <div className="blank-article"><BookOpen size={22}/><p>В этой статье пока нет текста.</p><button className="button primary" onClick={beginEdit}><Pencil size={14}/>Добавить содержание</button></div>}</div>}{selected.deleted_at && <div className="deleted-content"><BookOpen size={20}/><span>Содержимое статьи скрыто, пока она удалена.</span></div>}
          <div className="article-end"><div className="end-line"/><div className="updated-note"><Check size={14}/>Вы дочитали до конца <span>·</span> Обновлено {prettyDate(selected.updated_at)}</div></div>
          {childArticles.length > 0 && <section className="related-section child-section"><h2><FolderTree size={17}/>Дочерние материалы <span className="count">{childArticles.length}</span></h2><div className="related-links">{childArticles.map(a => <button key={a.id} onClick={() => selectArticle(a.id)}>{a.title || 'Без названия'}<ArrowLeft size={13}/></button>)}</div></section>}
          {(links.outgoing.length > 0 || links.incoming.length > 0) && <section className="related-section"><h2><Link2 size={17}/>Связанные материалы</h2>{links.outgoing.length > 0 && <div><span className="related-label">Ссылки из статьи</span><div className="related-links">{links.outgoing.map(a => <button key={a.id} onClick={() => selectArticle(a.id)}>{a.title}<ArrowLeft size={13}/></button>)}</div></div>}{links.incoming.length > 0 && <div><span className="related-label">На эту статью ссылаются</span><div className="related-links">{links.incoming.map(a => <button key={a.id} onClick={() => selectArticle(a.id)}>{a.title}<ArrowLeft size={13}/></button>)}</div></div>}</section>}
          <section className="attachments-section"><div className="section-title"><h2><FileImage size={17}/>Файлы</h2>{!selected.deleted_at && <button className="button ghost small" onClick={() => uploadRef.current?.click()}><Upload size={14}/>Загрузить</button>}<input ref={uploadRef} type="file" hidden onChange={e => void uploadFile(e.target.files)}/></div>{attachments.length ? <div className="attachment-grid">{attachments.map(f => <div className="attachment-entry" key={f.id}><a className="attachment-card" href={f.url} target="_blank" rel="noreferrer">{f.mime.startsWith('image/') ? <img src={f.url} alt={f.name}/> : <span className="file-icon"><FileImage size={21}/></span>}<span><b>{f.name}</b><small>{bytes(f.size)}</small></span><ArrowDownToLine size={15}/></a>{!selected.deleted_at && <button type="button" className="attachment-delete" onClick={() => setPendingDelete(f)} aria-label={`Удалить вложение ${f.name}`} title="Удалить вложение"><X size={14}/></button>}</div>)}</div> : <p className="muted small-text">Здесь появятся изображения и документы статьи.</p>}</section>
          <ArticleComments key={selected.id} ref={commentsRef} articleId={selected.id} deleted={!!selected.deleted_at} toc={toc} theme={theme} author={author} onCount={onCommentCount} onAnchorCounts={setAnchorCounts} onNavigate={openInternal} />
        </article> : !spaceId && !busy && !booting ? <section className="empty-spaces"><Archive size={32}/><h1>Нет активных пространств</h1><p>Создайте новое пространство или восстановите одно из архива. Ваши материалы сохранены.</p><button className="button primary" onClick={() => setSpaceManager(true)}><Settings2 size={16}/>Управление пространствами</button></section> : <section className="welcome"><div className="welcome-orbit orbit-one"/><div className="welcome-orbit orbit-two"/><div className="welcome-icon"><BookOpen size={27}/></div><div className="eyebrow">{(activeSpace?.name || 'БАЗА ЗНАНИЙ').toLocaleUpperCase('ru')}</div><h1>Знания, которые<br/><span>работают вместе.</span></h1><p className="welcome-lead">Соберите заметки, исследования и инструкции в одной базе — готовой для вас и ваших ИИ-агентов.</p><div className="welcome-actions"><button className="button primary large" onClick={() => beginCreate()}><Plus size={17}/>{articles.length || busy ? 'Новая статья' : 'Создать первую статью'}</button><button className="button ghost large" onClick={() => setPanel('ai')}><Bot size={17}/>Подключить ИИ</button></div><div className="welcome-stats"><div><span className="stat-number">{articles.length}</span><span>материалов</span></div><i/><div><span className="stat-number">{spaces.length}</span><span>пространства</span></div><i/><div><span className="stat-mark"><span/>Доступно ИИ-агентам</span></div></div>{bootstrap && bootstrap.instructions.length > 0 && <div className="welcome-instructions"><span>С чего начать</span>{bootstrap.instructions.slice(0, 3).map(a => <button key={a.id} onClick={() => selectArticle(a.id)}><BookOpen size={14}/>{a.title}<ChevronRight size={14}/></button>)}</div>}{bootstrap && bootstrap.recent.length > 0 && <div className="welcome-instructions"><span>Недавние материалы</span>{bootstrap.recent.slice(0, 5).map(a => <button key={a.id} onClick={() => selectArticle(a.id)}><Clock3 size={14}/>{a.title}<ChevronRight size={14}/></button>)}</div>}</section>}
      </main>
      <aside className={`sidebar right-sidebar ${panel ? 'right-open' : ''}`}>
        {panel === 'ai' ? <div className="right-panel"><div className="panel-head"><div><span className="eyebrow">ИНТЕГРАЦИЯ</span><h3>Подключить ИИ</h3></div><button className="icon-button" onClick={() => setPanel(null)} aria-label="Закрыть"><X size={17}/></button></div><div className="ai-intro"><span className="ai-graphic"><Bot size={22}/></span><p>Дайте ИИ-агенту доступ к базе знаний через MCP или обычный HTTP API.</p></div><div className="connection-status"><span className="connection-dot"/><div><strong>Сервер подключён</strong><small>База знаний доступна по текущему адресу</small></div></div><div className="code-card"><div><span>MCP endpoint</span><Code2 size={14}/></div><code>{location.origin}/mcp</code><p>Streamable HTTP · JSON-RPC</p></div><div className="code-card"><div><span>Bootstrap</span><Code2 size={14}/></div><code>GET /api/bootstrap?space_id={spaceId}</code><p>Инструкции, активное пространство и недавние материалы</p></div><div className="tool-list"><span className="eyebrow">ДОСТУПНЫЕ ИНСТРУМЕНТЫ</span>{['wiki_bootstrap', 'wiki_search', 'wiki_read', 'wiki_write', 'wiki_history', 'wiki_restore', 'wiki_comment'].map(t => <div key={t}><Code2 size={13}/><code>{t}</code><Check size={13}/></div>)}</div><div className="ai-footnote"><ShieldCheck size={15}/><span>База не отправляет данные во внешние сервисы. ИИ подключается по вашему выбору.</span></div></div>
          : <div className="toc-panel"><div className="toc-header"><span className="eyebrow">НА ЭТОЙ СТРАНИЦЕ</span><span className="toc-header-actions"><button className="icon-button tiny" onClick={() => { setPanel(null); document.querySelector('.attachments-section')?.scrollIntoView({ behavior: smoothBehavior() }); }} title="Файлы статьи"><FileImage size={15}/></button><button className="icon-button tiny panel-close" onClick={() => setPanel(null)} aria-label="Закрыть"><X size={15}/></button></span></div>{selected ? <>{!editor && !historyView && <Toc headings={toc} commentCounts={anchorCounts} onNavigate={id => { urlReplace(articleUrl(selected.id, { anchor: id })); if (panel === 'toc') setPanel(null); }}/>}{!selected.deleted_at && !editor && <button className="toc-comment" onClick={() => openComment('')}><MessageCircle size={14}/>Новый комментарий</button>}<div className="toc-separator"/><div className="article-info"><span className="eyebrow">СВЕДЕНИЯ</span><div><Clock3 size={14}/>Создано {prettyDate(selected.created_at)}</div><div><Activity size={14}/>Обновлено {prettyDate(selected.updated_at)}</div><div><History size={14}/>Ревизия {selected.revision}</div>{!editor && <button onClick={openHistory}>Открыть историю <ChevronRight size={13}/></button>}</div><div className="toc-separator"/><button className="markdown-download" onClick={() => { window.open(`/api/articles/${encodeURIComponent(selected.id)}/markdown`, '_blank'); }}><ArrowDownToLine size={15}/>Скачать Markdown</button></> : <div className="toc-empty"><span className="empty-art"><BookOpen size={21}/></span><strong>Всё начинается<br/>с одной заметки</strong><p>Структурируйте идеи, сохраняйте решения и делитесь контекстом с ИИ.</p><button className="button ghost small" onClick={() => beginCreate()}><Plus size={14}/>Новая статья</button></div>}<div className="right-bottom"><span>AI Wiki</span><span>Личная база знаний</span></div></div>}
      </aside>
      {importOpen && <Suspense fallback={null}><ImportMarkdownDialog spaces={activeSpaces} projects={projects} corporate={session.mode === 'corporate'} defaultSpaceId={spaceId} author={session.mode === 'corporate' ? session.user.name : author.trim() || 'human'} onClose={() => setImportOpen(false)} onImported={a => { setImportOpen(false); setSpaceId(a.space_id); void loadIndex(a.space_id).then(() => loadTags(a.space_id)); selectArticle(a.id); setNotice('Статья импортирована из Markdown'); }}/></Suspense>}{spaceManager && <SpacesManager spaces={spaces.filter(s=>!s.limited)} draftSpaceIds={editor && draft ? [draft.space_id, selected?.space_id || ''] : []} onChange={updateSpace} onClose={() => setSpaceManager(false)}/>}
      {pendingDelete && pendingDelete.article_id === selected?.id && <div className="delete-dialog-backdrop"><div ref={deleteDialogRef} className="delete-dialog" role="alertdialog" aria-modal="true" aria-labelledby="delete-attachment-title" aria-describedby="delete-attachment-description" tabIndex={-1}><h2 id="delete-attachment-title">Удалить вложение?</h2><p className="delete-dialog-filename">{pendingDelete.name}</p><p id="delete-attachment-description">Файл будет удалён из статьи, и ссылки на него перестанут работать. Оригинал останется на сервере.</p><div><button ref={deleteCancelRef} className="button ghost" onClick={() => setPendingDelete(null)} disabled={Boolean(deletingAttachmentId)}>Отмена</button><button className="button danger" onClick={() => void deleteAttachment()} disabled={Boolean(deletingAttachmentId)}>{deletingAttachmentId ? 'Удаляем…' : 'Удалить'}</button></div></div></div>}
      {pendingDeleteArticle && pendingDeleteArticle.id === selected?.id && <div className="delete-dialog-backdrop"><div ref={deleteDialogRef} className="delete-dialog article-delete-dialog" role="alertdialog" aria-modal="true" aria-labelledby="delete-article-title" aria-describedby="delete-article-description" tabIndex={-1}><h2 id="delete-article-title">Удалить статью «{pendingDeleteArticle.title}»?</h2><p id="delete-article-description">Статья будет скрыта из дерева и поиска. История, файлы и комментарии останутся сохранены. Перед удалением переместите дочерние статьи в другое место.</p><div><button ref={deleteCancelRef} className="button ghost" onClick={() => setPendingDeleteArticle(null)} disabled={deletingArticle}>Отмена</button><button className="button danger" onClick={() => void deleteArticle()} disabled={deletingArticle}>{deletingArticle ? 'Удаляем…' : 'Удалить статью'}</button></div></div></div>}
    </div>
  </div>;
}
function AuthGate() {
  const [session, setSession] = useState<AuthSession | null>(null);
  const [error, setError] = useState('');
  const [logoutError, setLogoutError] = useState('');
  const [signedOut, setSignedOut] = useState(new URLSearchParams(location.search).get('signed_out') === '1');
  const loggingOut = useRef(false);
  const [expired, setExpired] = useState(false);
  const [loading, setLoading] = useState(true);
  const loadSession = async () => {
    setLoading(true); setError('');
    try {
      const response = await fetch('/api/me', { credentials: 'same-origin' });
      if (response.status === 401) {
        if (new URLSearchParams(location.search).get('signed_out') === '1') { setSignedOut(true); return; }
        location.assign('/auth/login'); return;
      }
      if (!response.ok) throw new Error(`Не удалось проверить вход (${response.status}).`);
      const value = await response.json() as AuthSession;
      if (value.mode !== 'trusted' && value.mode !== 'corporate') throw new Error('Сервер вернул неизвестный режим входа.');
      setCsrfToken(value.mode === 'corporate' ? value.csrf_token : '');
      setCorporateSession(value.mode === 'corporate');
      setDraftIdentity(value.mode === 'corporate' ? value.user.id : '', value.mode === 'corporate');
      setSession(value);
    } catch (e) { setError((e as Error).message || 'Не удалось подключиться к серверу.'); }
    finally { setLoading(false); }
  };
  useEffect(() => {
    setAuthExpiredHandler(() => setExpired(true));
    void loadSession();
    return () => setAuthExpiredHandler(null);
  }, []);
  const logout = async () => {
    if (loggingOut.current) return;
    loggingOut.current = true;
    try {
    if (session?.mode === 'corporate') {
      setLogoutError('');
      try { await client.logout(); } catch (e) { setLogoutError((e as Error).message || 'Сервер не подтвердил выход. Повторите попытку.'); return; }
      clearCorporateDrafts(session.user.id);
    }
    setCsrfToken(''); setCorporateSession(false); setDraftIdentity('', false);
    setSession(null); setExpired(false); setSignedOut(true);
    history.replaceState(null, '', '/?signed_out=1');
    } finally { loggingOut.current = false; }
  };
  if (signedOut && !session && !loading) return <main className="auth-gate" aria-live="polite"><div><BookOpen size={24}/><h1>Вы вышли из AI Wiki</h1><p>Сеанс завершён. Для продолжения войдите снова.</p><button className="button primary" onClick={() => location.assign('/auth/login')}>Войти</button><button className="button ghost" onClick={() => location.assign('/auth/login?reauth=1')}>Войти другим пользователем</button></div></main>;
  if (expired) return <main className="auth-gate" aria-live="polite"><div><BookOpen size={24}/><h1>Сессия завершилась</h1><p>Черновики этой вкладки сохранены под вашей учётной записью.</p><button className="button primary" onClick={() => location.assign('/auth/login')}>Войти снова</button></div></main>;
  if (!session) return <main className="auth-gate" aria-live="polite"><div><BookOpen size={24}/><h1>{loading ? 'Проверяем вход…' : 'Нет доступа к базе знаний'}</h1>{error && <p role="alert">{error}</p>}{!loading && <button className="button primary" onClick={() => void loadSession()}>Повторить</button>}</div></main>;
  return <App session={session} logoutError={logoutError} onLogout={() => void logout()}/>;
}
createRoot(document.getElementById('root')!).render(<React.StrictMode><AuthGate/></React.StrictMode>);
