// CodeMirror 6 setup for compose YAML and .env files, themed with CSS variables (style.css) so it
// follows the app's light/dark switch without rebuilding the editor.
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import { yaml } from '@codemirror/lang-yaml'
import { bracketMatching, foldGutter, foldKeymap, HighlightStyle, indentOnInput, indentUnit, StreamLanguage, syntaxHighlighting } from '@codemirror/language'
import { json } from '@codemirror/lang-json'
import { dockerFile } from '@codemirror/legacy-modes/mode/dockerfile'
import { javascript } from '@codemirror/legacy-modes/mode/javascript'
import { nginx } from '@codemirror/legacy-modes/mode/nginx'
import { properties } from '@codemirror/legacy-modes/mode/properties'
import { python } from '@codemirror/legacy-modes/mode/python'
import { shell } from '@codemirror/legacy-modes/mode/shell'
import { standardSQL } from '@codemirror/legacy-modes/mode/sql'
import { toml } from '@codemirror/legacy-modes/mode/toml'
import { xml } from '@codemirror/legacy-modes/mode/xml'
import { lintGutter, setDiagnostics, type Diagnostic } from '@codemirror/lint'
import { unifiedMergeView } from '@codemirror/merge'
import { highlightSelectionMatches, search, searchKeymap } from '@codemirror/search'
import { Compartment, EditorState, type Extension } from '@codemirror/state'
import {
  drawSelection, EditorView, highlightActiveLine, highlightActiveLineGutter, highlightSpecialChars, keymap, lineNumbers, placeholder,
} from '@codemirror/view'
import { tags as t } from '@lezer/highlight'

export type Lang = 'yaml' | 'env' | 'json' | 'dockerfile' | 'shell' | 'nginx' | 'toml' | 'ini' | 'python' | 'javascript' | 'xml' | 'sql' | 'text'

// langFor picks the highlighting for a file name.
export function langFor(path: string): Lang {
  const name = path.split('/').pop()!.toLowerCase()
  const ext = name.includes('.') ? name.slice(name.lastIndexOf('.') + 1) : ''
  if (name === 'dockerfile' || name.startsWith('dockerfile.') || ext === 'dockerfile' || name === 'containerfile') return 'dockerfile'
  if (name === '.env' || name.startsWith('.env.') || ext === 'env') return 'env'
  if (name === 'nginx.conf' || path.includes('nginx/') || ext === 'nginx') return 'nginx'
  const byExt: Record<string, Lang> = {
    yaml: 'yaml', yml: 'yaml', json: 'json', json5: 'json', sh: 'shell', bash: 'shell', zsh: 'shell', toml: 'toml',
    ini: 'ini', conf: 'ini', cfg: 'ini', properties: 'ini', py: 'python', js: 'javascript', mjs: 'javascript', ts: 'javascript',
    xml: 'xml', html: 'xml', svg: 'xml', sql: 'sql',
  }
  return byExt[ext] ?? 'text'
}

const theme = EditorView.theme({
  '&': { color: 'var(--cm-fg)', backgroundColor: 'var(--cm-bg)', fontSize: '12.5px', height: '100%' },
  '.cm-scroller': { fontFamily: 'var(--font-mono)', lineHeight: '1.6' },
  '.cm-content': { caretColor: 'var(--cm-caret)', padding: '8px 0' },
  '.cm-cursor, .cm-dropCursor': { borderLeftColor: 'var(--cm-caret)' },
  '&.cm-focused .cm-selectionBackground, .cm-selectionBackground, ::selection': { backgroundColor: 'var(--cm-selection) !important' },
  '.cm-gutters': { backgroundColor: 'var(--cm-gutter-bg)', color: 'var(--cm-gutter-fg)', border: 'none', borderRight: '1px solid var(--cm-border)' },
  '.cm-activeLine': { backgroundColor: 'var(--cm-active-line)' },
  '.cm-activeLineGutter': { backgroundColor: 'var(--cm-active-line)', color: 'var(--cm-fg)' },
  '.cm-foldPlaceholder': { backgroundColor: 'transparent', border: '1px solid var(--cm-border)', color: 'var(--cm-gutter-fg)' },
  '.cm-searchMatch': { backgroundColor: 'var(--cm-match)' },
  '.cm-selectionMatch': { backgroundColor: 'var(--cm-match)' },
  '.cm-panels': { backgroundColor: 'var(--cm-gutter-bg)', color: 'var(--cm-fg)', borderColor: 'var(--cm-border)' },
  '.cm-panels input, .cm-panels button': { fontSize: '12px' },
  '.cm-tooltip': { backgroundColor: 'var(--cm-gutter-bg)', border: '1px solid var(--cm-border)', color: 'var(--cm-fg)' },
  '.cm-diagnostic-error': { borderLeftColor: '#ef4444' },
  '.cm-lintRange-error': { backgroundImage: 'none', textDecoration: 'underline wavy #ef4444' },
  '.cm-collapsedLines': { background: 'var(--cm-gutter-bg)', color: 'var(--cm-gutter-fg)', borderTop: '1px solid var(--cm-border)', borderBottom: '1px solid var(--cm-border)', fontSize: '11px', padding: '2px 8px' },
  '.cm-collapsedLines:before, .cm-collapsedLines:after': { display: 'none' },
  '.cm-changedLine': { backgroundColor: 'var(--cm-added-line) !important' },
  '.cm-deletedChunk': { backgroundColor: 'var(--cm-deleted-line) !important' },
  '.cm-insertedLine, .cm-changedText': { backgroundColor: 'var(--cm-added) !important' },
  '.cm-deletedLine, .cm-deletedText': { backgroundColor: 'var(--cm-deleted) !important' },
})

const highlight = HighlightStyle.define([
  { tag: [t.propertyName, t.definition(t.propertyName), t.keyword], color: 'var(--cm-key)' },
  { tag: [t.string, t.special(t.string), t.attributeValue], color: 'var(--cm-string)' },
  { tag: [t.number, t.bool, t.null, t.atom], color: 'var(--cm-number)' },
  { tag: [t.comment, t.lineComment], color: 'var(--cm-comment)', fontStyle: 'italic' },
  { tag: [t.punctuation, t.separator, t.operator, t.meta], color: 'var(--cm-punct)' },
  { tag: [t.labelName, t.typeName, t.tagName], color: 'var(--cm-anchor)' },
])

function language(lang: Lang): Extension {
  switch (lang) {
    case 'yaml': return yaml()
    case 'json': return json()
    case 'env': case 'ini': return StreamLanguage.define(properties)
    case 'dockerfile': return StreamLanguage.define(dockerFile)
    case 'shell': return StreamLanguage.define(shell)
    case 'nginx': return StreamLanguage.define(nginx)
    case 'toml': return StreamLanguage.define(toml)
    case 'python': return StreamLanguage.define(python)
    case 'javascript': return StreamLanguage.define(javascript)
    case 'xml': return StreamLanguage.define(xml)
    case 'sql': return StreamLanguage.define(standardSQL)
  }
  return []
}

const base = (lang: Lang): Extension[] => [
  lineNumbers(),
  foldGutter(),
  highlightSpecialChars(),
  drawSelection(),
  indentOnInput(),
  bracketMatching(),
  highlightActiveLine(),
  highlightActiveLineGutter(),
  highlightSelectionMatches(),
  search({ top: true }),
  syntaxHighlighting(highlight),
  language(lang),
  indentUnit.of('  '),
  EditorState.tabSize.of(2),
  theme,
]

export interface EditorOptions {
  doc: string
  lang: Lang
  readOnly?: boolean
  onChange?: (doc: string) => void
  onSave?: () => void
  placeholder?: string
}

export function createEditor(parent: HTMLElement, o: EditorOptions) {
  const ro = new Compartment()
  const view = new EditorView({
    parent,
    state: EditorState.create({
      doc: o.doc,
      extensions: [
        ...base(o.lang),
        history(),
        lintGutter(),
        placeholder(o.placeholder ?? ''),
        keymap.of([
          { key: 'Mod-s', preventDefault: true, run: () => (o.onSave?.(), true) },
          indentWithTab,
          ...defaultKeymap,
          ...historyKeymap,
          ...foldKeymap,
          ...searchKeymap,
        ]),
        ro.of([EditorState.readOnly.of(!!o.readOnly), EditorView.editable.of(!o.readOnly)]),
        EditorView.updateListener.of((u) => u.docChanged && o.onChange?.(u.state.doc.toString())),
      ],
    }),
  })
  return {
    view,
    setDoc(doc: string) {
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: doc } })
    },
    setReadOnly(r: boolean) {
      view.dispatch({ effects: ro.reconfigure([EditorState.readOnly.of(r), EditorView.editable.of(!r)]) })
    },
    // mark a validation error on its line (1-based) or clear marks
    mark(line?: number, column?: number, message?: string) {
      const diags: Diagnostic[] = []
      if (line && message) {
        const l = view.state.doc.line(Math.min(Math.max(line, 1), view.state.doc.lines))
        const from = Math.min(l.from + Math.max((column ?? 1) - 1, 0), l.to)
        diags.push({ from, to: l.to > from ? l.to : from, severity: 'error', message })
      }
      view.dispatch(setDiagnostics(view.state, diags))
    },
    goto(line: number) {
      const l = view.state.doc.line(Math.min(Math.max(line, 1), view.state.doc.lines))
      view.dispatch({ selection: { anchor: l.from }, scrollIntoView: true })
      view.focus()
    },
    destroy: () => view.destroy(),
  }
}

// A read-only unified diff: the new text with deleted lines from the old one shown inline.
export function createDiff(parent: HTMLElement, original: string, modified: string, lang: Lang) {
  return new EditorView({
    parent,
    state: EditorState.create({
      doc: modified,
      extensions: [
        ...base(lang),
        EditorState.readOnly.of(true),
        EditorView.editable.of(false),
        unifiedMergeView({ original, mergeControls: false, highlightChanges: true, gutter: true, collapseUnchanged: { margin: 3, minSize: 6 } }),
      ],
    }),
  })
}
