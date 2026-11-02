/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { modifier } from 'ember-modifier';
import { bind } from '@ember/runloop';
import { htmlSafe } from '@ember/template';
import codemirror from 'codemirror';

import 'codemirror/addon/edit/matchbrackets';
import 'codemirror/addon/selection/active-line';
import 'codemirror/mode/javascript/javascript';
import 'codemirror/mode/ruby/ruby';

// HCL has no CodeMirror 5 mode; the Ruby mode highlights its strings,
// numbers, comments and blocks well enough (as the previous editor did).
const MODES = {
  hcl: 'ruby',
  json: { name: 'javascript', json: true },
  text: null,
};

/**
 * A code editor (CodeMirror 5) styled with the design tokens, light and
 * dark. One-way: @onChange receives the new text; a changed @value is
 * applied to the editor.
 *
 *   <Ui::CodeEditor @label="Job definition" @language="hcl"
 *     @value={{this.text}} @onChange={{this.setText}} @errorLines={{array 3}} />
 *
 * @language: hcl, json or text. @readOnly, @wrap (line wrapping),
 * @autofocus, @minHeight (CSS length), @errorLines (1-based lines to mark),
 * @onSubmit (Cmd/Ctrl+Enter).
 */
export default class UiCodeEditor extends Component {
  editor = null;

  get style() {
    return this.args.minHeight
      ? htmlSafe(`--norca-editor-min-height: ${this.args.minHeight}`)
      : null;
  }

  setup = modifier(
    (element, _positional, { value, language, readOnly, wrap, errorLines }) => {
      if (!this.editor) {
        this.editor = codemirror(element, {
          value: value || '',
          mode: MODES[language || 'text'],
          theme: 'norca',
          lineNumbers: true,
          matchBrackets: true,
          styleActiveLine: !readOnly,
          readOnly: readOnly ? true : false,
          lineWrapping: !!wrap,
          tabSize: 2,
          viewportMargin: Infinity,
          screenReaderLabel: this.args.label || 'Code editor',
          extraKeys: this.args.onSubmit
            ? {
                'Cmd-Enter': () => this.args.onSubmit?.(),
                'Ctrl-Enter': () => this.args.onSubmit?.(),
              }
            : {},
        });
        this.editor.on(
          'change',
          bind(this, (cm) => this.args.onChange?.(cm.getValue()))
        );
        if (this.args.autofocus) this.editor.focus();
      } else {
        if (value !== undefined && this.editor.getValue() !== (value || '')) {
          this.editor.setValue(value || '');
        }
        this.editor.setOption('mode', MODES[language || 'text']);
        this.editor.setOption('readOnly', readOnly ? true : false);
        this.editor.setOption('lineWrapping', !!wrap);
      }

      // Mark the lines with errors.
      const editor = this.editor;
      (this.markedLines || []).forEach((handle) =>
        editor.removeLineClass(handle, 'background', 'norca-cm-error-line')
      );
      this.markedLines = (errorLines || [])
        .filter((line) => line >= 1 && line <= editor.lineCount())
        .map((line) =>
          editor.addLineClass(line - 1, 'background', 'norca-cm-error-line')
        );
    }
  );

  willDestroy() {
    super.willDestroy(...arguments);
    this.editor = null;
  }
}
