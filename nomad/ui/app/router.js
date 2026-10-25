/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import EmberRouter from '@ember/routing/router';
import config from 'nomad-ui/config/environment';

export default class Router extends EmberRouter {
  location = config.locationType;
  rootURL = config.rootURL;
}

Router.map(function () {
  this.route('exec', { path: '/exec/:job_name' }, function () {
    this.route('task-group', { path: '/:task_group_name' }, function () {
      this.route('task', { path: '/:task_name' });
    });
  });

  this.route('jobs', function () {
    this.route('run', function () {
      this.route('templates', function () {
        this.route('new');
        this.route('manage');
        this.route('template', { path: '/:name' });
      });
    });
    this.route('job', { path: '/:job_name' }, function () {
      this.route('task-group', { path: '/:name' });
      this.route('definition');
      this.route('versions');
      this.route('deployments');
      this.route('dispatch');
      this.route('evaluations');
      this.route('allocations');
      this.route('clients');
      this.route('services', function () {
        this.route('service', { path: '/:name' });
      });
      this.route('variables');
    });
  });

  this.route('clients', function () {
    this.route('client', { path: '/:node_id' }, function () {
      this.route('monitor');
    });
  });

  this.route('servers', function () {
    this.route('server', { path: '/:agent_id' }, function () {
      this.route('monitor');
    });
  });

  this.route('topology');

  this.route('csi', function () {
    this.route('volumes', function () {
      this.route('volume', { path: '/:volume_name' });
    });

    this.route('plugins', function () {
      this.route('plugin', { path: '/:plugin_name' }, function () {
        this.route('allocations');
      });
    });
  });

  this.route('allocations', function () {
    this.route('allocation', { path: '/:allocation_id' }, function () {
      this.route('fs-root', { path: '/fs' });
      this.route('fs', { path: '/fs/*path' });

      this.route('task', { path: '/:name' }, function () {
        this.route('logs');
        this.route('fs-root', { path: '/fs' });
        this.route('fs', { path: '/fs/*path' });
      });
    });
  });

  this.route('settings', function () {
    this.route('tokens');
  });

  // if we don't include function() the outlet won't render
  this.route('evaluations', function () {});

  this.route('not-found', { path: '/*' });
  this.route('variables', function () {
    this.route('new');

    this.route(
      'variable',
      {
        path: '/var/*id',
      },
      function () {
        this.route('edit');
      }
    );

    this.route('path', {
      path: '/path/*absolutePath',
    });
  });

  this.route('access-control', function () {
    this.route('policies', function () {
      this.route('new');
      this.route('policy', {
        path: '/:name',
      });
    });
    this.route('roles', function () {
      this.route('new');
      this.route('role', {
        path: '/:id',
      });
    });
    this.route('tokens', function () {
      this.route('new');
      this.route('token', {
        path: '/:id',
      });
    });
  });
  this.route('backups', function () {
    this.route('job-history');
    this.route('request', { path: '/restores/:restore_id' });
    this.route('backup', { path: '/:backup_id' });
    this.route('restore', { path: '/:backup_id/restore' });
  });

  this.route('quotas', function () {
    this.route('new');
    this.route('quota', { path: '/:name' });
    this.route('edit', { path: '/:name/edit' });
  });

  this.route('scaling', function () {
    this.route('policy', { path: '/:policy_id' });
  });

  this.route('services', function () {
    this.route('service', { path: '/:name' });
  });

  this.route('network', function () {
    this.route('policy', { path: '/policies/:namespace/:name' });
    this.route('admin-policy', { path: '/admin/:name' });
  });

  this.route('deployments', function () {
    this.route('deployment', { path: '/:deployment_id' });
  });

  this.route('packages', function () {
    this.route('release', { path: '/:namespace/:name' });
  });

  this.route('objects', function () {
    this.route('object', { path: '/:group/:kind/:name' });
  });

  this.route('kinds', function () {
    this.route('kind', { path: '/:group/:kind' });
  });

  this.route('admission', function () {
    this.route('policy', { path: '/:name' });
  });

  // Mirage-only route for testing OIDC flow
  if (config['ember-cli-mirage']) {
    this.route('oidc-mock');
  }
});
