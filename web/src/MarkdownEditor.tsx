import { forwardRef, useEffect, useImperativeHandle, useRef } from 'react';
import { Compartment, EditorState, Transaction } from '@codemirror/state';
import { EditorView, keymap, lineNumbers, highlightActiveLineGutter } from '@codemirror/view';
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands';
import { markdown } from '@codemirror/lang-markdown';
import { syntaxHighlighting, HighlightStyle } from '@codemirror/language';
import { tags } from '@lezer/highlight';
import './MarkdownEditor.css';

export type MarkdownEditorHandle = { insertText(value: string): void; wrapSelection(before: string, after: string): void };
type Props = {
  value: string;
  onChange: (value: string) => void;
  theme: 'light' | 'dark';
  ariaLabel?: string;
  compact?: boolean;
  readOnly?: boolean;
};

const colors = {
  light: {
    text: '#253044', muted: '#778196', accent: '#365fc7', heading: '#274a9b',
    link: '#315db8', quote: '#65738a', code: '#a13b50', strong: '#263e70',
  },
  dark: {
    text: '#e0e6f0', muted: '#8793a8', accent: '#8caeff', heading: '#a9c1ff',
    link: '#9fc0ff', quote: '#a1adc0', code: '#ff9ba8', strong: '#d4e0ff',
  },
} as const;

function themeExtension(theme: 'light' | 'dark') {
  const c = colors[theme];
  const highlight = HighlightStyle.define([
    { tag: tags.heading, color: c.heading, fontWeight: '700' },
    { tag: tags.heading1, fontSize: '1.35em' },
    { tag: tags.heading2, fontSize: '1.2em' },
    { tag: tags.strong, color: c.strong, fontWeight: '700' },
    { tag: tags.emphasis, fontStyle: 'italic' },
    { tag: tags.link, color: c.link, textDecoration: 'underline' },
    { tag: tags.url, color: c.link },
    { tag: tags.monospace, color: c.code },
    { tag: tags.quote, color: c.quote, fontStyle: 'italic' },
    { tag: tags.processingInstruction, color: c.muted },
  ]);
  return [syntaxHighlighting(highlight), EditorView.theme({
    '&': { color: c.text, backgroundColor: 'transparent' },
    '.cm-content': { caretColor: c.accent },
    '.cm-cursor, .cm-dropCursor': { borderLeftColor: c.accent },
    '&.cm-focused .cm-selectionBackground, .cm-selectionBackground': { backgroundColor: theme === 'dark' ? '#38558a88' : '#b9cbf588' },
    '.cm-activeLine': { backgroundColor: theme === 'dark' ? '#ffffff08' : '#263b5a08' },
    '.cm-gutters': { color: c.muted, backgroundColor: 'transparent', border: 'none' },
    '.cm-activeLineGutter': { color: c.accent, backgroundColor: 'transparent' },
  }, { dark: theme === 'dark' })];
}

export const MarkdownEditor = forwardRef<MarkdownEditorHandle, Props>(function MarkdownEditor({ value, onChange, theme, ariaLabel = 'Содержимое Markdown', compact = false, readOnly = false }, ref) {
  const hostRef = useRef<HTMLDivElement>(null);
  const viewRef = useRef<EditorView | null>(null);
  const themeRef = useRef<{ compartment: Compartment; extension: ReturnType<typeof themeExtension> } | null>(null);
  const editableRef = useRef<Compartment | null>(null);
  const onChangeRef = useRef(onChange);
  const externalValueRef = useRef(value);
  onChangeRef.current = onChange;

  useImperativeHandle(ref, () => ({
    insertText(inserted) {
      const view = viewRef.current;
      if (!view || !inserted) return;
      view.dispatch(view.state.replaceSelection(inserted), { scrollIntoView: true });
      view.focus();
    },
    wrapSelection(before, after) {
      const view = viewRef.current;
      if (!view) return;
      const { from, to } = view.state.selection.main;
      view.dispatch({ changes: [{ from, to, insert: `${before}${view.state.doc.sliceString(from, to)}${after}` }], selection: { anchor: from + before.length, head: to + before.length }, scrollIntoView: true });
      view.focus();
    },
  }), []);

  useEffect(() => {
    if (!hostRef.current) return;
    const compartment = new Compartment();
    const editable = new Compartment();
    const state = EditorState.create({
      doc: value,
      extensions: [
        lineNumbers(), highlightActiveLineGutter(), EditorView.lineWrapping, markdown(), history(),
        keymap.of([...defaultKeymap, ...historyKeymap, indentWithTab]),
        compartment.of(themeExtension(theme)),
        editable.of([EditorState.readOnly.of(readOnly), EditorView.editable.of(!readOnly)]),
        EditorView.updateListener.of(update => {
          if (update.docChanged) {
            const next = update.state.doc.toString();
            externalValueRef.current = next;
            onChangeRef.current(next);
          }
        }),
        EditorView.contentAttributes.of({ 'aria-label': ariaLabel, spellcheck: 'false' }),
      ],
    });
    const view = new EditorView({ state, parent: hostRef.current });
    viewRef.current = view;
    themeRef.current = { compartment, extension: themeExtension(theme) };
    editableRef.current = editable;
    return () => {
      view.destroy();
      viewRef.current = null;
      themeRef.current = null;
      editableRef.current = null;
    };
  // The editor is created once; current props are synchronized below.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;
    if (value !== externalValueRef.current && value !== view.state.doc.toString()) {
      externalValueRef.current = value;
      view.dispatch({
        changes: { from: 0, to: view.state.doc.length, insert: value },
        annotations: Transaction.addToHistory.of(false),
      });
    }
  }, [value]);

  useEffect(() => {
    const view = viewRef.current;
    const stored = themeRef.current;
    if (!view || !stored) return;
    const extension = themeExtension(theme);
    view.dispatch({ effects: stored.compartment.reconfigure(extension) });
    themeRef.current = { compartment: stored.compartment, extension };
  }, [theme]);

  useEffect(() => {
    const view = viewRef.current; const compartment = editableRef.current;
    if (view && compartment) view.dispatch({ effects: compartment.reconfigure([EditorState.readOnly.of(readOnly), EditorView.editable.of(!readOnly)]) });
  }, [readOnly]);

  return <div className={`markdown-editor markdown-editor--${theme}${compact ? ' markdown-editor--compact' : ''}`} ref={hostRef} />;
});
