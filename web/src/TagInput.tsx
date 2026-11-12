import { useEffect, useRef, useState } from 'react';
import type { ClipboardEvent, KeyboardEvent } from 'react';
import { X } from 'lucide-react';
import './TagInput.css';

type Props = {
  id: string;
  value: string;
  options: readonly string[];
  onChange: (value: string) => void;
};

type Entry = { value: string; tags: string[]; text: string };

const tagKey = (tag: string) => tag.trim().toLocaleLowerCase('ru');
const parseTags = (value: string) => value.split(',').map(tag => tag.trim()).filter(Boolean);

function appendTags(tags: string[], additions: string[]) {
  const known = new Set(tags.map(tagKey));
  const next = [...tags];
  for (const addition of additions) {
    const tag = addition.trim();
    const key = tagKey(tag);
    if (!key || known.has(key)) continue;
    known.add(key);
    next.push(tag);
  }
  return next;
}

export function TagInput({ id, value, options, onChange }: Props) {
  const [entry, setEntry] = useState<Entry | null>(null);
  const [showSuggestions, setShowSuggestions] = useState(false);
  const [highlight, setHighlight] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLUListElement>(null);

  // Incoming saved or restored values represent completed tags. Only a value
  // echoed back from this field can still contain the current input token.
  if (entry && entry.value !== value) setEntry(null);
  const current = entry?.value === value ? entry : null;
  const tags = current?.tags ?? parseTags(value);
  const token = current?.text ?? '';
  const needle = tagKey(token);
  const known = new Set(tags.map(tagKey));
  const suggestions: string[] = [];
  if (needle) {
    for (const option of options) {
      const key = tagKey(option);
      if (!key.includes(needle) || known.has(key)) continue;
      known.add(key);
      suggestions.push(option);
    }
  }
  const expanded = showSuggestions && suggestions.length > 0;
  const highlightedIndex = highlight === null ? -1 : suggestions.indexOf(highlight);
  const listId = `${id}-suggestions`;
  const activeId = expanded && highlightedIndex >= 0 ? `${listId}-${highlightedIndex}` : undefined;

  useEffect(() => {
    if (activeId) listRef.current?.querySelector('[aria-selected="true"]')?.scrollIntoView({ block: 'nearest' });
  }, [activeId]);

  function update(nextTags: string[], text = '') {
    // The parent also uses this string for draft recovery and Ctrl+S, so the
    // pending tag must reach it as the user types.
    const nextValue = appendTags(nextTags, [text]).join(', ');
    setEntry(text ? { value: nextValue, tags: nextTags, text } : null);
    if (nextValue !== value) onChange(nextValue);
  }

  function commit(text: string) {
    update(appendTags(tags, parseTags(text)));
    setShowSuggestions(false);
    setHighlight(null);
  }

  function changeText(text: string) {
    const pieces = text.split(',');
    const pending = pieces.pop() ?? '';
    update(appendTags(tags, pieces), pending);
    setShowSuggestions(true);
    setHighlight(null);
  }

  function handlePaste(event: ClipboardEvent<HTMLInputElement>) {
    const pasted = event.clipboardData.getData('text/plain');
    if (!pasted.includes(',')) return;
    const input = event.currentTarget;
    event.preventDefault();
    commit(token.slice(0, input.selectionStart ?? token.length) + pasted.replace(/[\r\n]+/g, ' ') + token.slice(input.selectionEnd ?? token.length));
  }

  function handleKeyDown(event: KeyboardEvent<HTMLInputElement>) {
    if (event.nativeEvent.isComposing || event.ctrlKey || event.metaKey || event.altKey) return;

    switch (event.key) {
      case 'ArrowDown':
      case 'ArrowUp': {
        if (!suggestions.length) return;
        event.preventDefault();
        const down = event.key === 'ArrowDown';
        const index = highlightedIndex < 0
          ? (down ? 0 : suggestions.length - 1)
          : (highlightedIndex + (down ? 1 : -1) + suggestions.length) % suggestions.length;
        setShowSuggestions(true);
        setHighlight(suggestions[index]);
        break;
      }
      case 'Enter':
      case ',':
        event.preventDefault();
        if (token) commit(event.key === 'Enter' && activeId ? suggestions[highlightedIndex] : token);
        break;
      case 'Escape':
        if (!expanded) return;
        event.preventDefault();
        event.stopPropagation();
        setShowSuggestions(false);
        setHighlight(null);
        break;
      case 'Backspace':
        if (!token && tags.length) {
          event.preventDefault();
          update(tags.slice(0, -1));
        }
        break;
    }
  }

  return <div className="tag-input">
    <div className="tag-input-box" onClick={event => {
      if (event.target === event.currentTarget) inputRef.current?.focus();
    }}>
      {tags.map((tag, index) => <span className="tag-input-chip" key={`${index}-${tag}`}>
        {tag}
        <button type="button" aria-label={`Удалить тег «${tag}»`} onMouseDown={event => event.preventDefault()} onClick={() => {
          update(tags.filter((_, tagIndex) => tagIndex !== index), token);
          inputRef.current?.focus();
        }}><X size={13} aria-hidden="true"/></button>
      </span>)}
      <input id={id} ref={inputRef} className="tag-input-entry" role="combobox" autoComplete="off"
        aria-autocomplete="list" aria-expanded={expanded} aria-controls={expanded ? listId : undefined} aria-activedescendant={activeId}
        value={token} placeholder={tags.length ? 'Добавить тег' : 'Введите тег'}
        onChange={event => changeText(event.target.value)} onFocus={() => setShowSuggestions(true)}
        onBlur={() => {
          if (token) commit(token);
          else setShowSuggestions(false);
        }}
        onKeyDown={handleKeyDown} onPaste={handlePaste}/>
    </div>
    {expanded && <ul className="tag-input-suggestions" id={listId} ref={listRef} role="listbox" aria-label="Подсказки по тегам" onMouseDown={event => event.preventDefault()}>
      {suggestions.map((tag, index) => <li key={tag} id={`${listId}-${index}`} role="option" aria-selected={index === highlightedIndex}
        onClick={() => commit(tag)}>{tag}</li>)}
    </ul>}
  </div>;
}
