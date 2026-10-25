/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-env node */

let USE_MIRAGE = true;

if (process.env.USE_MIRAGE) {
  USE_MIRAGE = process.env.USE_MIRAGE == 'true';
}

let USE_PERCY = true;

if (process.env.USE_PERCY) {
  USE_PERCY = process.env.USE_PERCY == 'true';
}

module.exports = function (environment) {
  let ENV = {
    modulePrefix: 'nomad-ui',
    environment: environment,
    rootURL: '/ui/',
    locationType: 'history',
    EmberENV: {
      FEATURES: {
        // Here you can enable experimental features on an ember canary build
        // e.g. EMBER_NATIVE_DECORATOR_SUPPORT: true
      },
      EXTEND_PROTOTYPES: {
        // Prevent Ember Data from overriding Date.parse.
        Date: false,
      },
    },

    APP: {
      blockingQueries: true,
      mirageScenario: 'smallCluster',
      mirageWithNamespaces: true,
      mirageWithTokens: true,
      mirageWithRegions: true,
    },

    percy: {
      enabled: USE_PERCY,
    },
  };

  if (environment === 'development') {
    // ENV.APP.LOG_RESOLVER = true;
    // ENV.APP.LOG_ACTIVE_GENERATION = true;
    // ENV.APP.LOG_TRANSITIONS = true;
    // ENV.APP.LOG_TRANSITIONS_INTERNAL = true;
    // ENV.APP.LOG_VIEW_LOOKUPS = true;

    ENV['ember-cli-mirage'] = {
      enabled: USE_MIRAGE,
      excludeFilesFromBuild: !USE_MIRAGE,
    };

    // Mirage cannot stream: the event stream client (services/event-stream)
    // only connects to a real agent.
    ENV.APP.eventStream = !USE_MIRAGE;
  }

  if (environment === 'test') {
    // Testem prefers this...
    ENV.locationType = 'none';

    // keep test console output quieter
    ENV.APP.LOG_ACTIVE_GENERATION = false;
    ENV.APP.LOG_VIEW_LOOKUPS = false;

    ENV.APP.rootElement = '#ember-testing';
    ENV.APP.autoboot = false;

    // Tests enable the event stream client explicitly with a stubbed request.
    ENV.APP.eventStream = false;

    ENV.browserify = {
      tests: true,
    };

    ENV['ember-cli-mirage'] = {
      trackRequests: true,
    };
  }

  // if (environment === 'production') {
  // }

  return ENV;
};
