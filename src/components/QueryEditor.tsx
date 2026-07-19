import React, { useCallback, useEffect, useRef, useState } from 'react';
import { QueryEditorProps, GrafanaTheme2 } from '@grafana/data';
import { Button, CodeEditor, Drawer, useStyles2, type Monaco } from '@grafana/ui';
import { css } from '@emotion/css';

import { DataSource } from '../datasource';
import { MyDataSourceOptions, MyQuery } from '../types';
import { docMap, hoverMarkdown, summarize } from './docmap';

type Props = QueryEditorProps<DataSource, MyQuery, MyDataSourceOptions>;

/**
 * Compact button + summary that opens a right-side Drawer hosting a
 * full-height Monaco Python editor for `query.source`.
 *
 * The source is committed (onChange → onRunQuery) on blur, on Cmd/Ctrl+S, and
 * when the drawer closes — never per keystroke. The live editor value is held
 * in a ref so closing the drawer always commits the latest text.
 */
export function QueryEditor({ query, onChange, onRunQuery, datasource }: Props) {
  const styles = useStyles2(getStyles);
  const [open, setOpen] = useState(false);
  const [shipped, setShipped] = useState('');
  const liveSource = useRef(query.source ?? '');

  const hasRef = !!query.ref;
  const hasOverride = (query.source ?? '') !== '';
  const displayValue = hasOverride ? (query.source ?? '') : shipped;

  const editorHeight = Math.max(
    320,
    (typeof window !== 'undefined' ? window.innerHeight : 900) - 130,
  );

  // Fetch the shipped metric for a referenced panel so it can be shown and so
  // "reset" has a baseline to fall back to.
  useEffect(() => {
    let cancelled = false;
    if (query.ref) {
      datasource
        .fetchMetricSource(query.ref)
        .then((s) => {
          if (!cancelled) {
            setShipped(s);
          }
        })
        .catch(() => {
          if (!cancelled) {
            setShipped('');
          }
        });
    }
    return () => {
      cancelled = true;
    };
  }, [query.ref, datasource]);

  // Re-seed the live ref whenever the drawer opens, from whatever is currently
  // shown (override if present, else the shipped baseline).
  useEffect(() => {
    if (open) {
      liveSource.current = displayValue;
    }
  }, [open, displayValue]);

  const commit = useCallback(
    (source: string) => {
      liveSource.current = source;
      // Editing a referenced panel writes an override; identical-to-shipped is
      // treated as no override so the panel stays clean.
      const nextSource = hasRef && source === shipped ? '' : source;
      if (nextSource === (query.source ?? '')) {
        return;
      }
      onChange({ ...query, source: nextSource });
      onRunQuery();
    },
    [query, onChange, onRunQuery, hasRef, shipped],
  );

  const reset = useCallback(() => {
    liveSource.current = shipped;
    if ((query.source ?? '') !== '') {
      onChange({ ...query, source: '' });
      onRunQuery();
    }
  }, [query, onChange, onRunQuery, shipped]);

  const onBeforeEditorMount = useCallback((monaco: Monaco) => {
    registerPythonHover(monaco);
  }, []);

  return (
    <div className={styles.wrap}>
      <Button
        variant="secondary"
        icon="brackets-curly"
        onClick={() => setOpen(true)}
        aria-label="Edit Code"
      >
        Edit Code
      </Button>
      {!hasRef && (
        <span className={styles.summary} title={query.source ?? ''}>
          {summarize(query.source)}
        </span>
      )}

      {open && (
        <Drawer
          title="Python source"
          subtitle="Hover a metric function for its signature and docstring."
          size="md"
          scrollableContent={false}
          onClose={() => {
            commit(liveSource.current);
            setOpen(false);
          }}
        >
          {hasRef && hasOverride && (
            <div className={styles.drawerToolbar}>
              <Button variant="secondary" fill="text" icon="history" onClick={reset}>
                Restore Default
              </Button>
            </div>
          )}
          <div style={{ height: editorHeight, width: '100%' }}>
            <CodeEditor
              value={displayValue}
              language="python"
              width="100%"
              height={editorHeight}
              showMiniMap={false}
              showLineNumbers
              monacoOptions={{ automaticLayout: false, scrollBeyondLastLine: false }}
              onBeforeEditorMount={onBeforeEditorMount}
              onEditorDidMount={(editor) => {
                const relayout = () => {
                  const node = editor.getContainerDomNode?.();
                  if (!node) {
                    return;
                  }
                  let el: HTMLElement | null = node;
                  for (let i = 0; i < 4 && el; i++) {
                    el.style.height = `${editorHeight}px`;
                    el = el.parentElement;
                  }
                  const w = node.clientWidth || Math.round(window.innerWidth * 0.45);
                  editor.layout({ width: w, height: editorHeight });
                };
                relayout();
                setTimeout(relayout, 60);
                setTimeout(relayout, 250);
                window.addEventListener('resize', relayout);
                editor.onDidDispose(() => window.removeEventListener('resize', relayout));
              }}
              onChange={(v) => {
                liveSource.current = v;
              }}
              onBlur={(v) => commit(v)}
              onSave={(v) => commit(v)}
            />
          </div>
        </Drawer>
      )}
    </div>
  );
}

let hoverRegistered = false;

/**
 * Register a native Monaco hover provider for Python that resolves the word
 * under the cursor against the bundled metric doc-map. Returns nothing on a
 * miss (builtins, locals). Idempotent across editor mounts.
 */
function registerPythonHover(monaco: Monaco) {
  if (hoverRegistered) {
    return;
  }
  hoverRegistered = true;
  monaco.languages.registerHoverProvider('python', {
    provideHover(model, position) {
      const word = model.getWordAtPosition(position);
      if (!word) {
        return null;
      }
      const md = hoverMarkdown(word.word, docMap);
      if (!md) {
        return null;
      }
      return {
        range: new monaco.Range(
          position.lineNumber,
          word.startColumn,
          position.lineNumber,
          word.endColumn,
        ),
        contents: [{ value: md }],
      };
    },
  });
}

const getStyles = (theme: GrafanaTheme2) => ({
  wrap: css`
    display: flex;
    align-items: center;
    gap: ${theme.spacing(1)};
    flex-wrap: wrap;
  `,
  summary: css`
    font-family: ${theme.typography.fontFamilyMonospace};
    font-size: ${theme.typography.bodySmall.fontSize};
    color: ${theme.colors.text.secondary};
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 60%;
  `,
  drawerToolbar: css`
    display: flex;
    justify-content: flex-end;
    margin-bottom: ${theme.spacing(1)};
  `,
});
