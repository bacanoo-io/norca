/**
 * Copyright (c) HashiCorp, Inc.
 * Modifications Copyright (c) 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable ember/no-controller-access-in-routes */
import { inject as service } from '@ember/service';
import { later, next } from '@ember/runloop';
import Route from '@ember/routing/route';
import { AbortError } from '@ember-data/adapter/error';
import RSVP from 'rsvp';
import { action } from '@ember/object';
import classic from 'ember-classic-decorator';

@classic
export default class ApplicationRoute extends Route {
  @service config;
  @service system;
  @service store;
  @service token;
  @service router;

  queryParams = {
    region: {
      refreshModel: true,
    },
  };

  // The routes a visitor without a token may open on a cluster with ACLs:
  // the sign-in page, the return of an SSO sign-in (the tokens page with
  // the code and state of the provider) and the mock provider of the tests.
  // The substates (loading, error) of a transition are not pages.
  isOpen(transition) {
    const name = transition.to?.name || '';
    if (!name || /(^|[._-])(loading|error)$/.test(name)) return true;
    const to = transition.to;
    if (to.name === 'login' || to.name === 'oidc-mock') return true;
    return (
      to.name === 'settings.tokens' &&
      !!to.queryParams?.code &&
      !!to.queryParams?.state
    );
  }

  // Sends a visitor without a token to the sign-in page, and remembers the
  // page asked for. Returns whether it did.
  gate(transition) {
    if (!this.token.needsSignIn || this.isOpen(transition)) return false;
    const url = transition.intent?.url;
    if (url && !url.startsWith('/login')) this.token.loginRedirect = url;
    transition.abort();
    this.router.transitionTo('login');
    return true;
  }

  init() {
    super.init(...arguments);
    // Every transition after the start is checked too: a sign-out, an
    // expired token, a link followed without a token.
    this.router.on('routeWillChange', (transition) => {
      if (!transition.queryParamsOnly) this.gate(transition);
    });
  }

  resetController(controller, isExiting) {
    if (isExiting) {
      controller.set('error', null);
    }
  }

  async beforeModel(transition) {
    let promises;

    // service:router#transitionTo can cause this to rerun because of refreshModel on
    // the region query parameter, this skips rerunning the detection/loading queries.
    if (transition.queryParamsOnly) {
      promises = Promise.resolve(true);
    } else {
      let exchangeOneTimeToken;

      if (transition.to.queryParams.ott) {
        exchangeOneTimeToken = this.get('token').exchangeOneTimeToken(
          transition.to.queryParams.ott
        );
      } else {
        exchangeOneTimeToken = Promise.resolve(true);
      }

      try {
        await exchangeOneTimeToken;
      } catch (e) {
        this.controllerFor('application').set('error', e);
      }

      const fetchSelfTokenAndPolicies = await this.get(
        'token.fetchSelfTokenAndPolicies'
      )
        .perform()
        .catch();

      const checkFuzzySearchPresence = this.get(
        'system.checkFuzzySearchPresence'
      )
        .perform()
        .catch();

      promises = await RSVP.all([
        this.get('system.regions'),
        this.get('system.defaultRegion'),
        fetchSelfTokenAndPolicies,
        checkFuzzySearchPresence,
      ]);

      // The token is known now: without one, a cluster with ACLs shows only
      // the sign-in page.
      this.token.set('checked', true);
      if (this.gate(transition)) return promises;
    }

    if (!this.get('system.shouldShowRegions')) return promises;

    const queryParam = transition.to.queryParams.region;
    const defaultRegion = this.get('system.defaultRegion.region');
    const currentRegion = this.get('system.activeRegion') || defaultRegion;

    // Only reset the store if the region actually changed
    if (
      (queryParam && queryParam !== currentRegion) ||
      (!queryParam && currentRegion !== defaultRegion)
    ) {
      this.store.unloadAll();
    }

    this.set('system.activeRegion', queryParam || defaultRegion);

    return promises;
  }

  // Model is being used as a way to propagate the region and
  // one time token query parameters for use in setupController.
  model(
    { region },
    {
      to: {
        queryParams: { ott },
      },
    }
  ) {
    return {
      region,
      hasOneTimeToken: ott,
    };
  }

  setupController(controller, { region, hasOneTimeToken }) {
    if (region === this.get('system.defaultRegion.region')) {
      next(() => {
        controller.set('region', null);
      });
    }

    super.setupController(...arguments);

    if (hasOneTimeToken) {
      // Hack to force clear the OTT query parameter
      later(() => {
        controller.set('oneTimeToken', '');
      }, 500);
    }
  }

  @action
  didTransition() {
    if (!this.get('config.isTest')) {
      window.scrollTo(0, 0);
    }
  }

  @action
  willTransition() {
    this.controllerFor('application').set('error', null);
  }

  @action
  error(error) {
    if (!(error instanceof AbortError)) {
      if (
        error.errors?.any(
          (e) =>
            e.detail === 'ACL token expired' ||
            e.detail === 'ACL token not found'
        )
      ) {
        this.token.reset();
        this.router.transitionTo('login');
      } else {
        this.controllerFor('application').set('error', error);
      }
    }
  }
}
