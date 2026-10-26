/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { modifier } from 'ember-modifier';
import moment from 'moment';
import { summarizeEvent } from 'nomad-ui/utils/event-summary';

const LIMIT = 200;
const TOPICS = ['Job', 'Deployment', 'Evaluation', 'Allocation', 'Scaling'];

/**
 * The Events tab of the job page (UI plan, job details): the unified event
 * stream filtered to the job: its registrations, deployments, evaluations,
 * allocations and scaling decisions. The events still buffered by the
 * servers are replayed first, then new ones arrive live.
 */
export default class JobsEvents extends Component {
  @service eventStream;

  @tracked events = [];
  @tracked filter = 'all';

  get job() {
    return this.args.job;
  }

  get namespace() {
    return this.job.get('namespace.name') || 'default';
  }

  get streamState() {
    return this.eventStream.stateFor(this.job.region || '', this.namespace);
  }

  get isEnabled() {
    return this.eventStream.enabled;
  }

  get filterOptions() {
    return [
      { value: 'all', label: 'All events' },
      ...TOPICS.map((t) => ({ value: t, label: t })),
    ];
  }

  get items() {
    return this.events
      .filter((e) => this.filter === 'all' || e.Topic === this.filter)
      .map((event) => {
        const item = summarizeEvent(event);
        return {
          ...item,
          time: event.receivedAt
            ? moment(event.receivedAt).format('HH:mm:ss')
            : '',
        };
      });
  }

  setFilter = (value) => {
    this.filter = value;
  };

  live = modifier(() => {
    const id = this.job.plainId;
    const unsubscribe = this.eventStream.subscribe({
      topics: TOPICS.map((t) => `${t}:${id}`),
      region: this.job.region || '',
      namespace: this.namespace,
      replay: true,
      onEvents: (events) => {
        const now = Date.now();
        const mine = events
          .filter((e) => this.belongsToJob(e))
          .map((e) => ({ ...e, receivedAt: now }));
        if (mine.length) {
          this.events = [...mine.reverse(), ...this.events].slice(0, LIMIT);
        }
      },
      onResync: () => {
        this.events = [];
        return 0;
      },
    });
    return () => unsubscribe();
  });

  // The topic filters match keys and filter keys; keep only the events of
  // this job (another job may have an allocation or evaluation ID equal to
  // the job name only in theory, but jobs of other namespaces may share it).
  belongsToJob(event) {
    const p = event.Payload || {};
    const jobId =
      p.Job?.ID ??
      p.Deployment?.JobID ??
      p.Evaluation?.JobID ??
      p.Allocation?.JobID ??
      p.JobID ??
      event.Key;
    const namespace = event.Namespace || p.Namespace || 'default';
    return jobId === this.job.plainId && namespace === this.namespace;
  }
}
