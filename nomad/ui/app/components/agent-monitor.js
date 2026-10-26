/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { inject as service } from '@ember/service';
import Component from '@ember/component';
import { action, computed } from '@ember/object';
import { capitalize } from '@ember/string';
import { guidFor } from '@ember/object/internals';
import { assert } from '@ember/debug';
import { tagName } from '@ember-decorators/component';
import classic from 'ember-classic-decorator';
import Log from 'nomad-ui/utils/classes/log';

const LEVELS = ['error', 'warn', 'info', 'debug', 'trace'];

@classic
@tagName('')
export default class AgentMonitor extends Component {
  @service token;

  client = null;
  server = null;
  level = LEVELS[2];
  onLevelChange() {}

  levels = LEVELS;
  levelOptions = LEVELS.map((level) => ({
    value: level,
    label: capitalize(level),
  }));
  monitorUrl = '/v1/agent/monitor';
  isStreaming = true;
  logger = null;

  @computed('client.id', 'level', 'server.{id,region}')
  get monitorParams() {
    assert(
      'Provide a client OR a server to AgentMonitor, not both.',
      this.server != null || this.client != null
    );

    const type = this.server ? 'server_id' : 'client_id';
    const id = this.server ? this.server.id : this.client && this.client.id;

    const params = {
      log_level: this.level,
      [type]: id,
    };

    if (this.server) {
      params.region = this.server.region;
    }

    return params;
  }

  get id() {
    return guidFor(this);
  }

  didInsertElement() {
    super.didInsertElement(...arguments);
    this.updateLogger();
  }

  updateLogger() {
    let currentTail = this.logger ? this.logger.tail : '';
    if (currentTail) {
      currentTail += `\n...changing log level to ${this.level}...\n\n`;
    }
    this.set(
      'logger',
      Log.create({
        logFetch: (url) => this.token.authorizedRequest(url),
        params: this.monitorParams,
        url: this.monitorUrl,
        tail: currentTail,
      })
    );
  }

  @action
  setLevel(level) {
    this.logger.stop();
    this.set('level', level);
    this.onLevelChange(level);
    this.updateLogger();
  }

  @action
  toggleStream() {
    this.set('streamMode', 'streaming');
    this.toggleProperty('isStreaming');
  }
}
