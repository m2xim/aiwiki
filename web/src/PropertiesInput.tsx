import { Fragment, useCallback, useLayoutEffect, useRef, useState } from 'react';
import './PropertiesInput.css';

type PropertiesInputProps = {
  id: string;
  value: string;
  onChange: (value: string) => void;
  invalid?: boolean;
  describedBy?: string;
};

export function PropertiesInput({ id, value, onChange, invalid = false, describedBy }: PropertiesInputProps) {
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const mirrorRef = useRef<HTMLSpanElement>(null);
  const [composing, setComposing] = useState(false);

  const syncMirror = useCallback(() => {
    const input = inputRef.current;
    const mirror = mirrorRef.current;
    if (!input || !mirror) return;

    // Match the available text width when a native scrollbar takes up space.
    mirror.style.right = `${input.offsetWidth - input.clientWidth - 1}px`;
    mirror.scrollTop = input.scrollTop;
    mirror.scrollLeft = input.scrollLeft;
  }, []);

  useLayoutEffect(syncMirror, [value, composing, syncMirror]);
  useLayoutEffect(() => {
    const input = inputRef.current;
    if (!input) return;
    const observer = new ResizeObserver(syncMirror);
    observer.observe(input);
    return () => observer.disconnect();
  }, [syncMirror]);

  // The native textarea displays CRLF and CR as line breaks too.
  const displayValue = value.replace(/\r\n?/g, '\n');

  return (
    <span className="properties-input" data-composing={composing || undefined}>
      <span className="properties-input-mirror" ref={mirrorRef} aria-hidden="true">
        {displayValue.split('\n').map((line, index) => {
          const separator = line.indexOf('=');
          return (
            <Fragment key={index}>
              {index > 0 && '\n'}
              <span className="properties-input-key">{separator < 0 ? line : line.slice(0, separator)}</span>
              {separator >= 0 && <>{'='}<span className="properties-input-value">{line.slice(separator + 1)}</span></>}
            </Fragment>
          );
        })}
        {displayValue.endsWith('\n') && '\u200b'}
      </span>
      <textarea
        id={id}
        ref={inputRef}
        className={`properties-area${invalid ? ' invalid' : ''}`}
        value={value}
        onChange={event => onChange(event.currentTarget.value)}
        onScroll={syncMirror}
        onCompositionStart={() => setComposing(true)}
        onCompositionEnd={() => setComposing(false)}
        aria-invalid={invalid || undefined}
        aria-describedby={describedBy}
        placeholder={'owner=команда\nsource=https://example.org'}
        rows={2}
      />
    </span>
  );
}
