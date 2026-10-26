/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { guidFor } from '@ember/object/internals';

/**
 *   <Ui::SavedViews @listId="jobs" @state={{this.viewState}} @onApply={{this.applyView}} />
 *
 * @state is the current state of the list (a plain object that survives
 * JSON); choosing a view calls @onApply with the saved state.
 */
export default class UiSavedViews extends Component {
  @service listPreferences;

  @tracked isSaving = false;
  @tracked isManaging = false;
  @tracked name = '';
  @tracked nameError = null;

  formId = `${guidFor(this)}-form`;

  get views() {
    return this.listPreferences.viewsFor(this.args.listId);
  }

  @action
  apply(view) {
    this.args.onApply?.(JSON.parse(JSON.stringify(view.state || {})));
  }

  @action
  openSave() {
    this.name = '';
    this.nameError = null;
    this.isSaving = true;
  }

  @action
  closeSave() {
    this.isSaving = false;
  }

  @action
  setName(value) {
    this.name = value;
    this.nameError = null;
  }

  @action
  save(event) {
    event?.preventDefault();
    const name = this.name.trim();
    if (!name) {
      this.nameError = 'Enter a name for the view.';
      return;
    }
    const state = JSON.parse(JSON.stringify(this.args.state || {}));
    this.listPreferences.saveView(this.args.listId, name, state);
    this.isSaving = false;
  }

  @action
  openManage() {
    this.isManaging = true;
  }

  @action
  closeManage() {
    this.isManaging = false;
  }

  @action
  delete(view) {
    this.listPreferences.deleteView(this.args.listId, view.name);
  }
}
