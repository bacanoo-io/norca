/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';
import { inject as service } from '@ember/service';
import { hash } from 'rsvp';
import notifyError from 'nomad-ui/utils/notify-error';
import { kind } from 'nomad-ui/utils/access-control/kinds';

/**
 * Route classes of the Access Control section, built from the record type
 * descriptions instead of being written out per type. The route files only
 * pick the right builder.
 */

// Already loaded records of the related types, keyed as the page expects.
function peekRelated(store, related = {}) {
  const out = {};
  for (const [key, type] of Object.entries(related)) {
    out[key] = store.peekAll(type);
  }
  return out;
}

/**
 * The section itself: anyone who may list at least one of the three record
 * types gets in, and the model holds the live collections the overview and
 * the list pages share.
 */
export class SectionRoute extends Route {
  @service can;
  @service router;
  @service store;

  static TYPES = ['policy', 'role', 'token'];

  listable(type) {
    return this.can.can(`list ${kind(type).plural}`);
  }

  beforeModel() {
    if (!SectionRoute.TYPES.some((type) => this.listable(type))) {
      this.router.transitionTo('/jobs');
    }
  }

  model() {
    const collections = {};
    for (const type of SectionRoute.TYPES) {
      collections[kind(type).plural] = this.listable(type)
        ? this.store.findAll(type, { reload: true })
        : [];
    }
    return hash(collections).catch(notifyError(this));
  }
}

/**
 * The "new record" page of a type: needs write access, starts from an
 * unsaved draft and throws the draft away when the page is left unsaved.
 */
export function draftRoute(type) {
  const spec = kind(type);
  return class extends Route {
    @service can;
    @service router;
    @service store;

    beforeModel() {
      if (this.can.cannot(`write ${type}`)) {
        this.router.transitionTo(spec.section);
      }
    }

    model() {
      return {
        [type]: this.store.createRecord(type, { ...spec.draft }),
        ...peekRelated(this.store, spec.newRelated),
      };
    }

    resetController(controller, isExiting) {
      const draft = controller.model?.[type];
      if (isExiting && draft?.isNew) {
        draft.destroyRecord();
      }
    }
  };
}

/**
 * The page of one saved record: always fetched fresh (list entries can be
 * stubs, e.g. tokens without their secret), with unsaved attribute edits
 * dropped when the page is left.
 */
export function recordRoute(type) {
  const spec = kind(type);
  return class extends Route {
    @service store;

    model(params) {
      return hash({
        [type]: this.store.findRecord(type, params.id, { reload: true }),
        ...peekRelated(this.store, spec.related),
      }).catch(notifyError(this));
    }

    resetController(controller, isExiting) {
      super.resetController(...arguments);
      const record = controller.model?.[type];
      if (
        isExiting &&
        record &&
        !record.isDeleted &&
        record.hasDirtyAttributes
      ) {
        record.rollbackAttributes();
      }
    }
  };
}
